package cgv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	seatmappb "github.com/cineko-org/contracts/v3/gen/go/cineko/seatmap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const seatMapResponseTimeout = 8 * time.Second

// CaptureSeatMap visits a current showtime only as provider request context and
// returns the auditorium-scoped static layout.
func (adapter *Adapter) CaptureSeatMap(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) (*seatmappb.Snapshot, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := validateSeatMapTask(task); err != nil {
		return nil, err
	}
	seatTask := task.GetSeatMap()
	if err := adapter.selectCinemaTheater(seatTask.GetTheater().GetRegion(), seatTask.GetTheater().GetName()); err != nil {
		return nil, err
	}
	showtime, err := adapter.resolveSeatMapShowtime(seatTask, ScheduleTheater{
		ID: seatTask.GetTheater().GetId(), ProviderID: seatTask.GetTheater().GetProviderId(),
		SourceKey: siteNoForTheater(seatTask.GetTheater()), Region: seatTask.GetTheater().GetRegion(), Name: seatTask.GetTheater().GetName(),
	})
	if err != nil {
		return nil, err
	}
	// Static layout capture is a browser request, not a booking-page click. The
	// current showtime exists only to satisfy CGV's provider request parameters;
	// the returned layout remains scoped to the auditorium.
	body, err := adapter.fetchSeatMapData(ctx, showtime)
	if err != nil {
		return nil, err
	}
	layout, err := parseSeatMapLayout(body, seatTask.GetAuditorium().GetId())
	if err != nil {
		return nil, err
	}
	return seatMapSnapshot(seatTask.GetAuditorium().GetId(), layout)
}

func (adapter *Adapter) fetchSeatMapData(ctx context.Context, showtime ScheduleShowtime) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity := strings.Split(showtime.SourceKey, "/")
	if len(identity) != 4 || identity[0] == "" || identity[1] == "" || identity[2] == "" || identity[3] == "" {
		return nil, fmt.Errorf("%w: seat-map provider context is incomplete", ErrIdentityMismatch)
	}
	query := url.Values{
		"coCd":       {"A420"},
		"siteNo":     {identity[0]},
		"scnYmd":     {strings.ReplaceAll(identity[1], "-", "")},
		"scnsNo":     {identity[2]},
		"scnSseq":    {identity[3]},
		"seatAreaNo": {""},
		"cusgdCd":    {""},
		"custNo":     {""},
	}
	requestPath := seatMapResponsePath + "?" + query.Encode()
	var response struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	expression := fmt.Sprintf(`(async () => {
		const controller = new AbortController();
		const timer = window.setTimeout(() => controller.abort(), %d);
		try {
			const response = await fetch(%s, {cache: 'no-store', credentials: 'same-origin', signal: controller.signal});
			return {status: response.status, body: await response.text()};
		} finally {
			window.clearTimeout(timer);
		}
	})()`, seatMapResponseTimeout.Milliseconds(), jsString(requestPath))
	if err := adapter.evaluate(expression, &response); err != nil {
		return nil, adapter.handleProviderFailure(fmt.Errorf("%w: browser seat-map request: %w", ErrProviderTransport, err))
	}
	if response.Status < 200 || response.Status > 299 {
		return nil, adapter.handleProviderFailure(providerHTTPError(response.Status))
	}
	body := []byte(response.Body)
	if len(body) == 0 || len(body) > maxScheduleResponseBytes {
		return nil, fmt.Errorf("%w: browser seat-map response size is invalid", ErrProviderInvalidResult)
	}
	return body, nil
}

// CaptureSeatAvailability visits the supplied exact showtime and returns a
// complete live seat view with its static layout atomically embedded.
func (adapter *Adapter) CaptureSeatAvailability(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) (*seatmappb.LiveSeatObservation, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := validateSeatAvailabilityTask(task); err != nil {
		return nil, err
	}
	seatTask := task.GetSeatAvailability()
	theater := ScheduleTheater{
		ID: seatTask.GetTheater().GetId(), ProviderID: seatTask.GetTheater().GetProviderId(),
		SourceKey: siteNoForTheater(seatTask.GetTheater()), Region: seatTask.GetTheater().GetRegion(), Name: seatTask.GetTheater().GetName(),
	}
	if err := adapter.selectCinemaTheater(theater.Region, theater.Name); err != nil {
		return nil, err
	}
	showtime, err := adapter.resolveSeatAvailabilityShowtime(seatTask, theater)
	if err != nil {
		return nil, err
	}
	if err := adapter.openNonmemberSeatPage(showtime); err != nil {
		return nil, err
	}
	adapter.resetProviderResponses()
	clicked, err := adapter.clickSeatMapRefresh()
	if err != nil {
		return nil, err
	}
	if !clicked {
		if loginRequired, loginErr := adapter.pageContains("CGV 회원 로그인이 필요한 서비스"); loginErr == nil && loginRequired {
			return nil, ErrAuthenticationRequired
		}
		return nil, fmt.Errorf("%w: seat refresh button not found", ErrUIContractChanged)
	}
	captured, err := adapter.waitForProviderResponse(ctx, seatMapResponsePath, seatMapResponseTimeout)
	if err != nil {
		return nil, err
	}
	if captured.err != nil {
		return nil, adapter.handleProviderFailure(captured.err)
	}
	layout, err := parseSeatMapLayout(captured.body, seatTask.GetAuditorium().GetId())
	if err != nil {
		return nil, err
	}
	availableIDs, err := parseSeatAvailability(captured.body, seatTask.GetAuditorium().GetId(), layout)
	if err != nil {
		return nil, err
	}
	return liveSeatObservation(seatTask.GetShowtime().GetId(), seatTask.GetAuditorium().GetId(), layout, availableIDs)
}

func liveSeatObservation(showtimeID, auditoriumID string, layout *seatmappb.Layout, availableIDs []string) (*seatmappb.LiveSeatObservation, error) {
	snapshot, err := seatMapSnapshot(auditoriumID, layout)
	if err != nil {
		return nil, err
	}
	observedAt := snapshot.GetObservedAt()
	availableSeats := make([]*seatmappb.AvailableSeat, 0, len(availableIDs))
	for _, seatID := range availableIDs {
		seat := &seatmappb.AvailableSeat{}
		seat.SetSeatId(seatID)
		availableSeats = append(availableSeats, seat)
	}
	availability := &seatmappb.AvailabilitySnapshot{}
	availability.SetShowtimeId(showtimeID)
	availability.SetAuditoriumId(auditoriumID)
	availability.SetLayoutHash(snapshot.GetLayoutHash())
	availability.SetAvailableSeats(availableSeats)
	availability.SetObservedAt(observedAt)
	result := &seatmappb.LiveSeatObservation{}
	result.SetLayout(snapshot)
	result.SetAvailability(availability)
	return result, nil
}

func seatMapSnapshot(auditoriumID string, layout *seatmappb.Layout) (*seatmappb.Snapshot, error) {
	hash, err := layoutHash(layout)
	if err != nil {
		return nil, fmt.Errorf("hash seat-map layout: %w", err)
	}
	capacity, err := seatCountAsInt32(len(layout.GetSeats()))
	if err != nil {
		return nil, errors.New("seat-map layout contains too many seats")
	}
	observedAt := timestamppb.New(time.Now().UTC())
	snapshot := &seatmappb.Snapshot{}
	snapshot.SetId(SeatMapVersionID(auditoriumID, hash))
	snapshot.SetAuditoriumId(auditoriumID)
	snapshot.SetLayoutHash(hash)
	snapshot.SetCapacity(capacity)
	snapshot.SetLayout(layout)
	snapshot.SetObservedAt(observedAt)
	return snapshot, nil
}

func seatCountAsInt32(value int) (int32, error) {
	if value < 0 || value > math.MaxInt32 {
		return 0, errors.New("seat count is outside int32 range")
	}
	return int32(value), nil //nolint:gosec // the range check above bounds this conversion
}

func layoutHash(layout *seatmappb.Layout) (string, error) {
	canonical, err := (proto.MarshalOptions{Deterministic: true}).Marshal(layout)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func validateSeatMapTask(task *observationpb.AssignmentTask) error {
	seatTask := task.GetSeatMap()
	if seatTask == nil || seatTask.GetAuditorium() == nil || seatTask.GetTheater() == nil {
		return fmt.Errorf("%w: seat-map assignment target is incomplete", ErrIdentityMismatch)
	}
	if err := validateSeatMapTheater(seatTask.GetTheater()); err != nil {
		return err
	}
	if err := validateSeatMapAuditorium(seatTask); err != nil {
		return err
	}
	if strings.TrimSpace(seatTask.GetTimeZone()) == "" {
		return fmt.Errorf("%w: seat-map assignment time zone is required", ErrIdentityMismatch)
	}
	return nil
}

// ValidateSeatMapTask checks the provider identity required before a live-seat
// browser operation is opened.
func ValidateSeatMapTask(task *observationpb.AssignmentTask) error {
	return validateSeatMapTask(task)
}

func validateSeatAvailabilityTask(task *observationpb.AssignmentTask) error {
	seatTask := task.GetSeatAvailability()
	if seatTask == nil || seatTask.GetTheater() == nil || seatTask.GetAuditorium() == nil || seatTask.GetShowtime() == nil {
		return fmt.Errorf("%w: seat-availability assignment target is incomplete", ErrIdentityMismatch)
	}
	if err := validateSeatMapTheater(seatTask.GetTheater()); err != nil {
		return err
	}
	if err := validateSeatMapAuditoriumIdentity(seatTask.GetTheater(), seatTask.GetAuditorium()); err != nil {
		return err
	}
	if err := validateSeatMapShowtime(seatTask.GetShowtime()); err != nil {
		return err
	}
	if err := validateSeatAvailabilityTarget(seatTask); err != nil {
		return err
	}
	_, date, _, _, validIdentity := ShowtimeIdentityValues(seatTask.GetShowtime())
	if !validIdentity {
		return fmt.Errorf("%w: showtime identity is not canonical", ErrIdentityMismatch)
	}
	if _, err := seatMapTargetDateValue(date); err != nil {
		return fmt.Errorf("seat-availability showtime schedule date: %w", err)
	}
	return nil
}

func validateSeatAvailabilityTarget(task *observationpb.SeatAvailabilityTask) error {
	showtime := task.GetShowtime()
	if showtime.GetTheaterId() != task.GetTheater().GetId() ||
		showtime.GetAuditorium() == nil || showtime.GetAuditorium().GetId() != task.GetAuditorium().GetId() ||
		showtime.GetAuditorium().GetTheaterId() != task.GetTheater().GetId() {
		return fmt.Errorf("%w: seat-availability showtime identity does not match target", ErrIdentityMismatch)
	}
	if strings.TrimSpace(task.GetLocale()) == "" || strings.TrimSpace(task.GetTimeZone()) == "" {
		return fmt.Errorf("%w: seat-availability locale and time zone are required", ErrIdentityMismatch)
	}
	return nil
}

// ValidateSeatAvailabilityTask checks the provider identity required before a
// live-seat browser operation is opened.
func ValidateSeatAvailabilityTask(task *observationpb.AssignmentTask) error {
	return validateSeatAvailabilityTask(task)
}

func validateSeatMapAuditoriumIdentity(theater *catalogpb.Theater, auditorium *catalogpb.Auditorium) error {
	siteNo, validTheater := TheaterSiteNo(theater)
	auditoriumSite, screenNo, validAuditorium := AuditoriumIdentityValues(auditorium)
	if theater == nil || auditorium == nil || !validTheater || !validAuditorium || auditorium.GetTheaterId() != theater.GetId() ||
		auditoriumSite != siteNo || auditorium.GetId() != CatalogID(ProviderCGV, "auditorium", auditoriumSite+"/"+screenNo) ||
		strings.TrimSpace(auditorium.GetName()) == "" {
		return fmt.Errorf("%w: seat-map auditorium identity is not canonical", ErrIdentityMismatch)
	}
	return nil
}

func validateSeatMapTheater(theater *catalogpb.Theater) error {
	siteNo, validIdentity := TheaterSiteNo(theater)
	if theater == nil || theater.GetProviderId() != ProviderCGV || !validIdentity ||
		strings.TrimSpace(theater.GetRegion()) == "" || strings.TrimSpace(theater.GetName()) == "" ||
		theater.GetId() != CatalogID(ProviderCGV, "theater", siteNo) {
		return fmt.Errorf("%w: seat-map theater identity is not canonical", ErrIdentityMismatch)
	}
	return nil
}

func validateSeatMapAuditorium(task *observationpb.SeatMapTask) error {
	if !validSeatMapAuditoriumIdentity(task) {
		return fmt.Errorf("%w: seat-map auditorium identity is not canonical", ErrIdentityMismatch)
	}
	return nil
}

func validSeatMapAuditoriumIdentity(task *observationpb.SeatMapTask) bool {
	return task != nil && validateSeatMapAuditoriumIdentity(task.GetTheater(), task.GetAuditorium()) == nil
}

// resolveSeatMapShowtime finds the first current showtime in the requested
// auditorium across every date CGV currently exposes. The showtime is provider request context only;
// the captured layout remains an auditorium-scoped resource.
func (adapter *Adapter) resolveSeatMapShowtime(
	task *observationpb.SeatMapTask,
	theater ScheduleTheater,
) (ScheduleShowtime, error) {
	targetDates, err := adapter.availableScheduleDates()
	if err != nil {
		return ScheduleShowtime{}, err
	}
	selectableDates := 0
	for _, date := range targetDates {
		if err := adapter.selectDate(date); err != nil {
			if errors.Is(err, ErrTargetDateUnavailable) {
				continue
			}
			return ScheduleShowtime{}, err
		}
		selectableDates++
		entries, err := adapter.extractSchedules(date, theater)
		if err != nil {
			return ScheduleShowtime{}, err
		}
		if showtime, found := firstSeatMapShowtime(entries, task.GetAuditorium().GetId()); found {
			return showtime, nil
		}
	}
	if selectableDates == 0 {
		return ScheduleShowtime{}, fmt.Errorf("%w: no CGV seat-map date was selectable", ErrTargetDateUnavailable)
	}
	return ScheduleShowtime{}, ErrNoBookableShowtime
}

func (adapter *Adapter) resolveSeatAvailabilityShowtime(
	task *observationpb.SeatAvailabilityTask,
	theater ScheduleTheater,
) (ScheduleShowtime, error) {
	_, dateValue, _, _, validIdentity := ShowtimeIdentityValues(task.GetShowtime())
	if !validIdentity {
		return ScheduleShowtime{}, fmt.Errorf("%w: showtime identity is not canonical", ErrIdentityMismatch)
	}
	date, err := seatMapTargetDateValue(dateValue)
	if err != nil {
		return ScheduleShowtime{}, err
	}
	if err := adapter.selectDate(date); err != nil {
		return ScheduleShowtime{}, err
	}
	entries, err := adapter.extractSchedules(date, theater)
	if err != nil {
		return ScheduleShowtime{}, err
	}
	return exactSeatMapShowtime(entries, task.GetShowtime())
}

func seatMapTargetDateValue(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("seat-map target date is missing")
	}
	return canonicalProviderDate(value)
}

// firstSeatMapShowtime preserves provider order while excluding unrelated
// auditoriums. Sold-out and zero-availability rows remain valid layout sources.
func firstSeatMapShowtime(entries []scheduleEntry, auditoriumID string) (ScheduleShowtime, bool) {
	return firstSeatMapShowtimeAt(entries, auditoriumID, time.Now().In(time.FixedZone("KST", 9*60*60)))
}

func firstSeatMapShowtimeAt(entries []scheduleEntry, auditoriumID string, now time.Time) (ScheduleShowtime, bool) {
	for _, entry := range entries {
		showtime := entry.Showtime
		if showtime.AuditoriumID != auditoriumID || showtime.SoldOut || showtime.AvailableSeats <= 0 {
			continue
		}
		startsAt, _, err := ParseShowtimeRange(showtime.Date, showtime.StartsAt, showtime.EndsAt, now.Location())
		if err == nil && startsAt.After(now.Add(5*time.Minute)) {
			return showtime, true
		}
	}
	return ScheduleShowtime{}, false
}

func validateSeatMapShowtime(showtime *catalogpb.Showtime) error {
	if !validSeatMapShowtimeIdentity(showtime) || !validSeatMapMovieIdentity(showtime.GetMovie()) ||
		!validSeatMapShowtimeAuditorium(showtime) || !validSeatMapShowtimeWindow(showtime) {
		return fmt.Errorf("%w: seat-map showtime identity is not canonical", ErrIdentityMismatch)
	}
	_, date, _, _, validIdentity := ShowtimeIdentityValues(showtime)
	if !validIdentity {
		return fmt.Errorf("%w: showtime identity is not canonical", ErrIdentityMismatch)
	}
	if _, err := seatMapTargetDateValue(date); err != nil {
		return fmt.Errorf("seat-map showtime schedule date: %w", err)
	}
	return nil
}

func validSeatMapShowtimeIdentity(showtime *catalogpb.Showtime) bool {
	siteNo, date, screenNo, sequence, validIdentity := ShowtimeIdentityValues(showtime)
	return showtime != nil && showtime.GetProviderId() == ProviderCGV && validIdentity &&
		showtime.GetId() == CatalogID(ProviderCGV, "showtime", showtimeSourceKey(siteNo, date, screenNo, sequence)) &&
		showtime.GetTheaterId() == CatalogID(ProviderCGV, "theater", siteNo)
}

func validSeatMapMovieIdentity(movie *catalogpb.Movie) bool {
	return movie != nil && movie.GetProviderId() == ProviderCGV && movie.GetIdentity() != nil && movie.GetIdentity().GetCgv() != nil &&
		numericIdentifier(strings.TrimSpace(movie.GetIdentity().GetCgv().GetMovieNo())) && strings.TrimSpace(movie.GetTitle()) != "" &&
		movie.GetId() == CatalogID(ProviderCGV, "movie", movie.GetIdentity().GetCgv().GetMovieNo())
}

func validSeatMapShowtimeAuditorium(showtime *catalogpb.Showtime) bool {
	auditorium := showtime.GetAuditorium()
	auditoriumSite, auditoriumScreen, validAuditorium := AuditoriumIdentityValues(auditorium)
	showtimeSite, _, showtimeScreen, _, validShowtime := ShowtimeIdentityValues(showtime)
	return auditorium != nil && validAuditorium && validShowtime &&
		auditoriumSite == showtimeSite && auditoriumScreen == showtimeScreen && strings.TrimSpace(auditorium.GetName()) != "" &&
		auditorium.GetId() == CatalogID(ProviderCGV, "auditorium", auditoriumSite+"/"+auditoriumScreen) &&
		auditorium.GetTheaterId() == showtime.GetTheaterId()
}

func validSeatMapShowtimeWindow(showtime *catalogpb.Showtime) bool {
	startsAt, endsAt := showtime.GetStartsAt(), showtime.GetEndsAt()
	_, _, _, _, validIdentity := ShowtimeIdentityValues(showtime)
	return validIdentity && startsAt != nil && endsAt != nil && startsAt.CheckValid() == nil &&
		endsAt.CheckValid() == nil && endsAt.AsTime().After(startsAt.AsTime())
}

func exactSeatMapShowtime(entries []scheduleEntry, command *catalogpb.Showtime) (ScheduleShowtime, error) {
	if command == nil || command.GetMovie() == nil || command.GetAuditorium() == nil {
		return ScheduleShowtime{}, fmt.Errorf("%w: exact showtime command is missing", ErrIdentityMismatch)
	}
	commandKey, validIdentity := showtimeProviderKey(command)
	if !validIdentity {
		return ScheduleShowtime{}, fmt.Errorf("%w: exact showtime identity is incomplete", ErrIdentityMismatch)
	}
	var matches []ScheduleShowtime
	for _, entry := range entries {
		if entry.Showtime.SourceKey == commandKey {
			matches = append(matches, entry.Showtime)
		}
	}
	if len(matches) != 1 {
		return ScheduleShowtime{}, fmt.Errorf("%w: expected one provider row for %s, got %d", ErrUIContractChanged, commandKey, len(matches))
	}
	match := matches[0]
	if match.ProviderID != ProviderCGV || match.ID != command.GetId() || match.TheaterID != command.GetTheaterId() ||
		match.MovieID != command.GetMovie().GetId() || match.AuditoriumID != command.GetAuditorium().GetId() {
		return ScheduleShowtime{}, fmt.Errorf("%w: provider showtime tuple changed", ErrUIContractChanged)
	}
	return match, nil
}

func showtimeProviderKey(showtime *catalogpb.Showtime) (string, bool) {
	siteNo, date, screenNo, sequence, validIdentity := ShowtimeIdentityValues(showtime)
	if !validIdentity {
		return "", false
	}
	return showtimeSourceKey(siteNo, date, screenNo, sequence), true
}

func siteNoForTheater(theater *catalogpb.Theater) string {
	value, _ := TheaterSiteNo(theater)
	return value
}

func (adapter *Adapter) openNonmemberSeatPage(showtime ScheduleShowtime) error {
	clicked, err := adapter.clickExactSeatMapShowtime(showtime)
	if err != nil {
		return err
	}
	if !clicked {
		return fmt.Errorf("%w: exact showtime %s was not found", ErrUIContractChanged, showtime.ID)
	}
	if err := adapter.wait(800 * time.Millisecond); err != nil {
		return err
	}
	nonmember, err := adapter.clickButtonExact("비회원 예매")
	if err != nil {
		return err
	}
	if nonmember {
		if err := adapter.wait(500 * time.Millisecond); err != nil {
			return err
		}
	}
	for _, marker := range []string{"CAPTCHA", "captcha", "자동입력 방지", "보안문자"} {
		protected, err := adapter.pageContains(marker)
		if err != nil {
			return err
		}
		if protected {
			return ErrCaptchaRequired
		}
	}
	return nil
}

func (adapter *Adapter) clickExactSeatMapShowtime(showtime ScheduleShowtime) (bool, error) {
	expression := fmt.Sprintf(`(() => {
		const expectedAuditorium = %s;
		const expectedStart = %s;
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		const clockDigits = value => normalize(value).replace(/[^0-9]/g, '');
		const matches = window.__cinekoQueryAll('button').filter(candidate => {
			if (candidate.disabled) return false;
			const buttonText = normalize(candidate.innerText || candidate.textContent);
			return buttonText.includes(normalize(expectedAuditorium)) &&
				clockDigits(buttonText).startsWith(clockDigits(expectedStart));
		});
		if (matches.length !== 1) return {count: matches.length, clicked: false};
		matches[0].scrollIntoView({block: 'center'});
		matches[0].click();
		return {count: 1, clicked: true};
	})()`, jsString(showtime.AuditoriumName), jsString(showtime.StartsAt))
	var result struct {
		Count   int  `json:"count"`
		Clicked bool `json:"clicked"`
	}
	if err := adapter.evaluate(expression, &result); err != nil {
		return false, err
	}
	if result.Count > 1 {
		return false, fmt.Errorf("%w: showtime display is ambiguous for %s", ErrUIContractChanged, showtime.SourceKey)
	}
	return result.Clicked, nil
}

func (adapter *Adapter) clickSeatMapRefresh() (bool, error) {
	const expression = `(() => {
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		const button = window.__cinekoQueryAll('button').find(item => {
			const label = normalize(item.getAttribute('aria-label') || item.title || item.innerText || item.textContent);
			return !item.disabled && (label.includes('새로고침') || label.toLowerCase().includes('refresh'));
		});
		if (!button) return false;
		button.click();
		return true;
	})()`
	var clicked bool
	err := adapter.evaluate(expression, &clicked)
	return clicked, err
}

func (adapter *Adapter) pageContains(text string) (bool, error) {
	expression := fmt.Sprintf(`(() => {
		const body = document.body && (document.body.innerText || document.body.textContent) || '';
		return body.includes(%s);
	})()`, jsString(text))
	var contains bool
	err := adapter.evaluate(expression, &contains)
	return contains, err
}

func (adapter *Adapter) waitForProviderResponse(
	ctx context.Context,
	path string,
	timeout time.Duration,
) (capturedProviderResponse, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		captures := adapter.takeProviderResponses(path)
		if len(captures) > 1 {
			return capturedProviderResponse{}, fmt.Errorf("%w: multiple seat-map responses", ErrUIContractChanged)
		}
		if len(captures) == 1 {
			return captures[0], nil
		}
		select {
		case <-ctx.Done():
			return capturedProviderResponse{}, ctx.Err()
		case <-adapter.ctx.Done():
			return capturedProviderResponse{}, adapter.ctx.Err()
		case <-deadline.C:
			return capturedProviderResponse{}, fmt.Errorf("timed out waiting for CGV seat-map response: %w", context.DeadlineExceeded)
		case <-ticker.C:
		}
	}
}
