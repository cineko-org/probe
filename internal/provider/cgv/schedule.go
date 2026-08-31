package cgv

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type scheduleEntry struct {
	Showtime       ScheduleShowtime
	AuditoriumName string
	ScreenTypes    []string
}

type ScheduleTheater struct {
	ID         string
	ProviderID string
	SourceKey  string
	Region     string
	Name       string
}

type ScheduleShowtime struct {
	ID                  string
	ProviderID          string
	SourceKey           string
	TheaterID           string
	MovieID             string
	MovieSourceKey      string
	MovieTitle          string
	PosterURL           string
	AuditoriumID        string
	AuditoriumSourceKey string
	AuditoriumName      string
	ScreenTypes         []string
	Date                string
	StartsAt            string
	EndsAt              string
	AvailableSeats      int
	Capacity            int
	SoldOut             bool
	ObservedAt          time.Time
	SourceLabel         string
}

type ScheduleCapture struct {
	TargetDate string
	Complete   bool
	Error      string
	Showtimes  []ScheduleShowtime
}

// CaptureSchedules returns a complete, unfiltered snapshot for every requested
// date. When none are requested, it scans every date currently exposed by CGV.
// Date-level failures stay attached to that date so an unavailable page can
// never be mistaken for evidence that no showtimes existed.
func (adapter *Adapter) CaptureSchedules(
	ctx context.Context,
	theater ScheduleTheater,
	requestedDates []string,
) ([]ScheduleCapture, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.captureSchedules(ctx, theater, requestedDates, nil)
}

// CaptureSchedulesForWeekdays scans only provider dates that can contain a
// monitor's selected weekdays. The caller includes the preceding provider day
// when extended-hour screenings (for example 25:30) can cross midnight.
func (adapter *Adapter) CaptureSchedulesForWeekdays(
	ctx context.Context,
	theater ScheduleTheater,
	weekdays []time.Weekday,
) ([]ScheduleCapture, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.captureSchedules(ctx, theater, nil, weekdays)
}

// CaptureScheduleWeekdayShard refreshes one provider date from the matching
// set. It keeps new-schedule detection bounded when many weeks share the same
// target weekdays.
func (adapter *Adapter) CaptureScheduleWeekdayShard(
	ctx context.Context,
	theater ScheduleTheater,
	weekdays []time.Weekday,
	shard int,
) ([]ScheduleCapture, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := adapter.providerRateLimitError(bookingCinemaURL); err != nil {
		return nil, err
	}
	if err := adapter.selectCinemaTheater(theater.Region, theater.Name); err != nil {
		return nil, err
	}
	dates, err := adapter.availableScheduleDates()
	if err != nil {
		return nil, err
	}
	dates, err = filterScheduleDatesByWeekdays(dates, weekdays)
	if err != nil {
		return nil, err
	}
	if len(dates) == 0 {
		return []ScheduleCapture{}, nil
	}
	if shard < 0 {
		shard = 0
	}
	return adapter.captureSelectedSchedules(ctx, theater, []string{dates[shard%len(dates)]})
}

func (adapter *Adapter) captureSchedules(
	ctx context.Context,
	theater ScheduleTheater,
	requestedDates []string,
	weekdays []time.Weekday,
) ([]ScheduleCapture, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := adapter.providerRateLimitError(bookingCinemaURL); err != nil {
		return nil, err
	}
	if err := adapter.selectCinemaTheater(theater.Region, theater.Name); err != nil {
		return nil, err
	}
	scanDates := requestedDates
	if len(scanDates) == 0 {
		var err error
		scanDates, err = adapter.availableScheduleDates()
		if err != nil {
			return nil, err
		}
		if len(weekdays) > 0 {
			scanDates, err = filterScheduleDatesByWeekdays(scanDates, weekdays)
			if err != nil {
				return nil, err
			}
		}
	}
	return adapter.captureSelectedSchedules(ctx, theater, scanDates)
}

func (adapter *Adapter) captureSelectedSchedules(
	ctx context.Context,
	theater ScheduleTheater,
	scanDates []string,
) ([]ScheduleCapture, error) {
	if adapter.logger != nil {
		adapter.logger.DebugContext(ctx, "CGV schedule capture started",
			"event", "cgv.schedule.capture.started",
			"scenario", "schedule_collection",
			"operation", "capture_theater_schedule",
			"outcome", "started",
			"theater_id", theater.ID,
			"theater_source_key", theater.SourceKey,
			"theater_name", theater.Name,
			"provider_dates", scanDates,
		)
	}
	result := make([]ScheduleCapture, 0, len(scanDates))
	for _, targetDate := range scanDates {
		capture := ScheduleCapture{TargetDate: targetDate}
		canonicalDate, err := canonicalProviderDate(targetDate)
		if err != nil {
			capture.Error = err.Error()
			adapter.logScheduleCaptureFailure(ctx, theater, targetDate, "canonicalize_date", err)
			result = append(result, capture)
			continue
		}
		capture.TargetDate = canonicalDate
		if err := adapter.requestScheduleDateFromPage(canonicalDate, theater.SourceKey); err != nil {
			if errors.Is(err, ErrProviderThrottled) {
				return result, err
			}
			capture.Error = err.Error()
			adapter.logScheduleCaptureFailure(ctx, theater, canonicalDate, "request_date", err)
			result = append(result, capture)
			continue
		}
		entries, err := adapter.extractSchedules(canonicalDate, theater)
		if err != nil {
			if errors.Is(err, ErrProviderThrottled) {
				return result, err
			}
			capture.Error = err.Error()
			adapter.logScheduleCaptureFailure(ctx, theater, canonicalDate, "extract_schedules", err)
			result = append(result, capture)
			continue
		}
		capture.Complete = true
		capture.Showtimes = make([]ScheduleShowtime, 0, len(entries))
		for _, entry := range entries {
			capture.Showtimes = append(capture.Showtimes, entry.Showtime)
		}
		if adapter.logger != nil {
			adapter.logger.DebugContext(ctx, "CGV schedule capture completed",
				"event", "cgv.schedule.capture.completed",
				"scenario", "schedule_collection",
				"operation", "capture_theater_schedule",
				"outcome", "succeeded",
				"theater_id", theater.ID,
				"theater_source_key", theater.SourceKey,
				"theater_name", theater.Name,
				"target_date", canonicalDate,
				"showtime_count", len(capture.Showtimes),
			)
		}
		result = append(result, capture)
	}
	return result, nil
}

// requestScheduleDateFromPage asks only for the schedule payload Cineko
// consumes. It runs inside the already-open CGV page, so cookies, origin,
// browser identity, response capture, and the local rate-limit gate remain on
// the same browser boundary without replaying the page's unrelated grade and
// schedule-existence requests for every polling round.
func (adapter *Adapter) requestScheduleDateFromPage(isoDate, siteNo string) error {
	requestPath, err := scheduleRequestPath(isoDate, siteNo)
	if err != nil {
		return err
	}
	if err := adapter.providerRateLimitError("https://cgv.co.kr" + requestPath); err != nil {
		return err
	}
	adapter.resetProviderResponses()
	expression := fmt.Sprintf(`(async () => {
		const path = %s;
		if (window.location.origin !== 'https://cgv.co.kr') {
			throw new Error('CGV booking page origin changed');
		}
		const response = await window.fetch(path, {
			method: 'GET',
			headers: {accept: 'application/json'},
			credentials: 'same-origin',
			cache: 'no-store',
			redirect: 'error',
		});
		await response.arrayBuffer();
		return response.status;
	})()`, jsString(requestPath))
	var status int
	if err := adapter.evaluate(expression, &status); err != nil {
		return fmt.Errorf("request CGV schedule from booking page: %w", err)
	}
	if status < 200 || status > 299 {
		return adapter.handleProviderFailure(providerHTTPError(status))
	}
	if err := adapter.waitForScheduleResponseReady(isoDate, 2*time.Second); err != nil {
		return fmt.Errorf("wait for CGV schedule response: %w", err)
	}
	return nil
}

func scheduleRequestPath(isoDate, siteNo string) (string, error) {
	canonicalDate, err := canonicalProviderDate(isoDate)
	if err != nil {
		return "", err
	}
	siteNo = strings.TrimSpace(siteNo)
	if !providerSiteIdentifier(siteNo) {
		return "", fmt.Errorf("%w: invalid CGV theater siteNo %q", ErrIdentityMismatch, siteNo)
	}
	query := url.Values{}
	query.Set("coCd", "A420")
	query.Set("siteNo", siteNo)
	query.Set("scnYmd", strings.ReplaceAll(canonicalDate, "-", ""))
	query.Set("rtctlScopCd", "08")
	return scheduleResponsePath + "?" + query.Encode(), nil
}

func filterScheduleDatesByWeekdays(dates []string, weekdays []time.Weekday) ([]string, error) {
	selected := make(map[time.Weekday]struct{}, len(weekdays))
	for _, weekday := range weekdays {
		if weekday < time.Sunday || weekday > time.Saturday {
			return nil, fmt.Errorf("%w: invalid schedule weekday %d", ErrIdentityMismatch, weekday)
		}
		selected[weekday] = struct{}{}
	}
	if len(selected) == 0 {
		return append([]string(nil), dates...), nil
	}
	result := make([]string, 0, len(dates))
	for _, value := range dates {
		canonical, err := canonicalProviderDate(value)
		if err != nil {
			return nil, err
		}
		date, err := time.Parse(time.DateOnly, canonical)
		if err != nil {
			return nil, fmt.Errorf("parse provider schedule date %q: %w", value, err)
		}
		if _, included := selected[date.Weekday()]; included {
			result = append(result, canonical)
		}
	}
	return result, nil
}

// availableScheduleDates resolves every date currently exposed by CGV. A
// monitor has no expiry; each scan asks the provider for its current window so
// newly opened weeks are picked up without persisting an arbitrary horizon.
func (adapter *Adapter) availableScheduleDates() ([]string, error) {
	var labels []string
	if err := adapter.evaluate(`(() => window.__cinekoQueryAll('button')
		.filter(button => !button.disabled)
		.map(button => (button.innerText || button.textContent || '').replace(/\s+/g, ' ').trim())
		.filter(Boolean))()`, &labels); err != nil {
		return nil, fmt.Errorf("read CGV schedule dates: %w", err)
	}
	return availableScheduleDatesFromLabels(labels, time.Now().In(time.FixedZone("KST", 9*60*60)))
}

func availableScheduleDatesFromLabels(labels []string, now time.Time) ([]string, error) {
	remaining := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		remaining[normalize(label)] = struct{}{}
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	dates := make([]string, 0, 16)
	// Date buttons omit the year. Matching the nearest occurrence within a
	// complete Gregorian cycle maps every visible day/weekday label without
	// imposing a user-visible monitoring deadline.
	for offset := 0; offset < 400 && len(remaining) > 0; offset++ {
		candidate := today.AddDate(0, 0, offset)
		variants := dateSelectionLabels(candidate, today)
		matched := false
		for _, label := range variants {
			if _, exists := remaining[normalize(label)]; exists {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		dates = append(dates, candidate.Format(time.DateOnly))
		for _, label := range variants {
			delete(remaining, normalize(label))
		}
	}
	if len(dates) == 0 {
		return nil, fmt.Errorf("%w: no selectable schedule dates", ErrUIContractChanged)
	}
	return dates, nil
}

func (adapter *Adapter) logScheduleCaptureFailure(
	ctx context.Context,
	theater ScheduleTheater,
	targetDate string,
	stage string,
	err error,
) {
	if adapter.logger == nil {
		return
	}
	adapter.logger.ErrorContext(ctx, "CGV schedule capture failed",
		"event", "cgv.schedule.capture.failed",
		"scenario", "schedule_collection",
		"operation", "capture_theater_schedule",
		"outcome", "failed",
		"expected", "complete schedule capture for target date",
		"observed", "schedule capture stage failed",
		"stage", stage,
		"theater_id", theater.ID,
		"theater_source_key", theater.SourceKey,
		"theater_name", theater.Name,
		"target_date", targetDate,
		"error", err,
	)
}

func (adapter *Adapter) selectCinemaTheater(region, theaterName string) error {
	if adapter.selectedRegion == region && adapter.selectedTheater == theaterName {
		adapter.logReusedCinemaTheater(theaterName)
		return nil
	}
	if err := adapter.navigate(bookingCinemaURL); err != nil {
		return fmt.Errorf("open CGV cinema booking: %w", err)
	}
	if err := adapter.selectCinemaRegion(region); err != nil {
		return err
	}
	if err := adapter.wait(150 * time.Millisecond); err != nil {
		return err
	}
	clicked, err := adapter.clickButtonExact(theaterName)
	if err != nil {
		return err
	}
	if !clicked {
		return fmt.Errorf("%w: theater button %q not found", ErrUIContractChanged, theaterName)
	}
	if err := adapter.wait(200 * time.Millisecond); err != nil {
		return err
	}
	if exists, _ := adapter.buttonExists("극장선택"); exists {
		_, _ = adapter.clickButtonExact("극장선택")
	}
	if err := adapter.wait(800 * time.Millisecond); err != nil {
		return err
	}
	adapter.selectedTheater = theaterName
	adapter.selectedRegion = region
	return nil
}

func (adapter *Adapter) logReusedCinemaTheater(theaterName string) {
	if adapter.logger == nil {
		return
	}
	adapter.logger.DebugContext(adapter.ctx, "CGV schedule theater selection reused",
		"event", "cgv.schedule.theater.reused",
		"scenario", "schedule_collection",
		"operation", "select_theater",
		"outcome", "succeeded",
		"theater_name", theaterName,
	)
}

func (adapter *Adapter) selectCinemaRegion(region string) error {
	clicked, err := adapter.clickButtonPrefix(region + "(")
	if err != nil {
		return err
	}
	if !clicked {
		clicked, err = adapter.openFavoriteCinemaEditorAndSelectRegion(region)
		if err != nil {
			return err
		}
	}
	if !clicked {
		return fmt.Errorf("%w: region button %q not found", ErrUIContractChanged, region)
	}
	return nil
}

func (adapter *Adapter) openFavoriteCinemaEditorAndSelectRegion(region string) (bool, error) {
	opened, err := adapter.clickButtonExact("자주가는 CGV 목록 수정")
	if err != nil || !opened {
		return false, err
	}
	if err := adapter.wait(200 * time.Millisecond); err != nil {
		return false, err
	}
	return adapter.clickButtonPrefix(region + "(")
}

func (adapter *Adapter) selectDate(isoDate string) error {
	parsed, err := time.ParseInLocation(time.DateOnly, isoDate, time.FixedZone("KST", 9*60*60))
	if err != nil {
		return err
	}
	now := time.Now().In(time.FixedZone("KST", 9*60*60))
	labels := dateSelectionLabels(parsed, now)
	for _, label := range labels {
		selected, err := adapter.buttonSelected(label)
		if err != nil {
			return err
		}
		if selected {
			return nil
		}
	}
	adapter.resetProviderResponses()
	for _, label := range labels {
		clicked, err := adapter.clickButtonExact(label)
		if err != nil {
			return err
		}
		if clicked {
			if err := adapter.waitForScheduleResponseReady(isoDate, 2*time.Second); err != nil {
				return fmt.Errorf("wait for CGV schedule response: %w", err)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: target date %s", ErrTargetDateUnavailable, isoDate)
}

func dateSelectionLabels(parsed, now time.Time) []string {
	day := parsed.Format("02")
	weekdays := []string{"일", "월", "화", "수", "목", "금", "토"}
	weekday := weekdays[parsed.Weekday()]
	labels := []string{weekday + " " + day, weekday + day, day + " " + weekday, day + weekday}
	if now.In(parsed.Location()).Format(time.DateOnly) == parsed.Format(time.DateOnly) {
		labels = append([]string{"오늘 " + day, "오늘" + day, day + " 오늘", day + "오늘"}, labels...)
	}
	return labels
}

func (adapter *Adapter) extractSchedules(
	date string,
	theater ScheduleTheater,
) ([]scheduleEntry, error) {
	rows, err := adapter.captureScheduleRows()
	if err != nil {
		if errors.Is(err, errScheduleResponseMissing) {
			return nil, fmt.Errorf("%w: %w", ErrUIContractChanged, err)
		}
		return nil, err
	}
	canonicalDate, err := canonicalProviderDate(date)
	if err != nil {
		return nil, err
	}
	entries := make([]scheduleEntry, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	targetRows := 0
	targetDateRows := 0
	for _, row := range rows {
		// CGV can include linked venue rows in a response for one site (for
		// example 0013 together with P013). The assignment identity remains the
		// authority, so only rows for its exact site number belong to this capture.
		if row.SiteNo != strings.TrimSpace(theater.SourceKey) {
			continue
		}
		targetRows++
		if row.Date != canonicalDate {
			continue
		}
		targetDateRows++
		entry, err := scheduleEntryFromProviderRow(row, theater)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[entry.Showtime.ID]; duplicate {
			continue
		}
		seen[entry.Showtime.ID] = struct{}{}
		entries = append(entries, entry)
	}
	if len(rows) > 0 && targetRows == 0 {
		return nil, fmt.Errorf(
			"%w: CGV schedule response contained no rows for theater siteNo %q",
			ErrIdentityMismatch, strings.TrimSpace(theater.SourceKey),
		)
	}
	if targetRows > 0 && targetDateRows == 0 {
		return nil, fmt.Errorf(
			"%w: CGV schedule response contained no rows for date %q at theater siteNo %q",
			ErrIdentityMismatch, canonicalDate, strings.TrimSpace(theater.SourceKey),
		)
	}
	return entries, nil
}

func scheduleEntryFromProviderRow(row providerScheduleRow, theater ScheduleTheater) (scheduleEntry, error) {
	theaterSiteNo := strings.TrimSpace(theater.SourceKey)
	if theater.ProviderID != ProviderCGV || !providerSiteIdentifier(theaterSiteNo) ||
		theater.ID != CatalogID(ProviderCGV, "theater", theaterSiteNo) {
		return scheduleEntry{}, fmt.Errorf("%w: CGV theater identity is not canonical", ErrIdentityMismatch)
	}
	if row.SiteNo != theaterSiteNo {
		return scheduleEntry{}, fmt.Errorf("%w: CGV schedule row siteNo %q does not match theater identity", ErrIdentityMismatch, row.SiteNo)
	}
	auditoriumName, screenTypes := parseAuditorium("", row.AuditoriumName)
	if auditoriumName == "" {
		return scheduleEntry{}, fmt.Errorf("CGV schedule row %q has no auditorium display name", row.Sequence)
	}
	movieSource := movieSourceKey(row.MovieNo)
	auditoriumSource := auditoriumSourceKey(row.SiteNo, row.AuditoriumNo)
	showtimeSource := showtimeSourceKey(row.SiteNo, row.Date, row.AuditoriumNo, row.Sequence)
	showtime := ScheduleShowtime{
		ID: providerCatalogID("showtime", showtimeSource), ProviderID: ProviderCGV,
		SourceKey: showtimeSource, TheaterID: theater.ID,
		MovieID: providerCatalogID("movie", movieSource), MovieSourceKey: movieSource,
		MovieTitle: row.MovieTitle, PosterURL: "",
		AuditoriumID: providerCatalogID("auditorium", auditoriumSource), AuditoriumSourceKey: auditoriumSource,
		AuditoriumName: auditoriumName, ScreenTypes: screenTypes,
		Date: row.Date, StartsAt: row.StartClock, EndsAt: row.EndClock,
		AvailableSeats: row.Available, Capacity: row.Capacity,
		SoldOut: row.Available == 0, ObservedAt: time.Now(),
		SourceLabel: strings.Join([]string{row.StartClock, row.EndClock, row.MovieTitle, auditoriumName}, " "),
	}
	return scheduleEntry{Showtime: showtime, AuditoriumName: auditoriumName, ScreenTypes: screenTypes}, nil
}

func parseAuditorium(group, structuredName string) (string, []string) {
	group = normalize(group)
	structuredName = normalize(structuredName)
	types := detectScreenTypes(group + " " + structuredName)
	if structuredName != "" {
		return structuredName, types
	}
	if group == "" || group == "2D" || group == "3D" {
		return "", types
	}
	name := group
	for _, token := range []string{
		"SCREENX DOLBY ATMOS mix 2D", "IMAX LASER 2D", "ULTRA 4DX 2D",
		"SCREENX 2D", "4DX 2D", "DOLBY ATMOS 2D", "2D", "3D",
	} {
		name = strings.TrimSpace(strings.TrimSuffix(name, token))
	}
	return name, types
}

func detectScreenTypes(value string) []string {
	upper := strings.ToUpper(value)
	var types []string
	for _, screenType := range []string{
		"ULTRA 4DX", "SCREENX", "IMAX", "4DX", "DOLBY ATMOS", "PREMIUM",
		"CINE DE CHEF", "CGV아트하우스", "LASER", "2D", "3D",
	} {
		if strings.Contains(upper, strings.ToUpper(screenType)) {
			types = append(types, screenType)
		}
	}
	return types
}

func normalize(value string) string { return strings.Join(strings.Fields(value), " ") }
