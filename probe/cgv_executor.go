package probe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	seatmappb "github.com/cineko-org/contracts/v3/gen/go/cineko/seatmap"
	"github.com/cineko-org/probe/v2/internal/egress"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	cgvbrowser "github.com/cineko-org/probe/v2/internal/provider/cgv/browser"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type scheduleBrowser interface {
	CaptureSchedules(context.Context, cgv.ScheduleTheater, []string) ([]cgv.ScheduleCapture, error)
	Close()
}

type weekdayScheduleBrowser interface {
	CaptureSchedulesForWeekdays(context.Context, cgv.ScheduleTheater, []time.Weekday) ([]cgv.ScheduleCapture, error)
}

type weekdayShardScheduleBrowser interface {
	CaptureScheduleWeekdayShard(context.Context, cgv.ScheduleTheater, []time.Weekday, int) ([]cgv.ScheduleCapture, error)
}

type catalogBrowser interface {
	CaptureCatalog(context.Context, []string) (cgv.CatalogCapture, error)
}

type seatMapBrowser interface {
	CaptureSeatMap(context.Context, *observationpb.AssignmentTask) (*seatmappb.Snapshot, error)
}

type seatMapCapture func(context.Context, *observationpb.AssignmentTask) (*seatmappb.Snapshot, error)

var errLocalExecution = errors.New("local scanner invariant failed")

type SeatMapExecutor interface {
	CaptureSeatMap(context.Context, *observationpb.AssignmentTask) (*seatmappb.Snapshot, error)
}

type CGVExecutor struct {
	open    func(context.Context, cgvbrowser.Task) (scheduleBrowser, error)
	clock   func() time.Time
	seatMap SeatMapExecutor
}

// ScheduleSession owns one anonymous managed-scan browser. It never carries a
// user session and can be reused across schedule rounds until the Soxy lease or
// browser becomes unhealthy.
type ScheduleSession struct {
	executor *CGVExecutor
	browser  scheduleBrowser
	mu       sync.Mutex
}

func (executor *CGVExecutor) CaptureSeatMap(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) (*seatmappb.Snapshot, error) {
	if task.GetSeatMap() == nil {
		return nil, fmt.Errorf("%w: unsupported Probe task kind", errLocalExecution)
	}
	if err := requireManagedScan(task); err != nil {
		return nil, err
	}
	if err := cgv.ValidateSeatMapTask(task); err != nil {
		return nil, err
	}
	if executor.seatMap != nil {
		return executor.seatMap.CaptureSeatMap(ctx, task)
	}
	if executor.open == nil {
		return nil, fmt.Errorf("%w: probe browser factory is unavailable", cgv.ErrBrowserStartFailed)
	}
	browserSession, err := executor.open(ctx, scanBrowserTask(task))
	if err != nil {
		return nil, fmt.Errorf("%w: open Probe browser: %w", cgv.ErrBrowserStartFailed, err)
	}
	defer browserSession.Close()
	capture, supported := seatMapBrowserCapture(browserSession)
	if !supported {
		return nil, fmt.Errorf("%w: probe browser does not support seat-map capture", errLocalExecution)
	}
	return capture(ctx, task)
}

func seatMapBrowserCapture(browser scheduleBrowser) (seatMapCapture, bool) {
	capture, supported := browser.(seatMapBrowser)
	if !supported {
		return nil, false
	}
	return capture.CaptureSeatMap, true
}

func NewCGVExecutor(factory *cgvbrowser.Factory) (*CGVExecutor, error) {
	if factory == nil {
		return nil, errors.New("probe browser factory is required")
	}
	return &CGVExecutor{
		open: func(ctx context.Context, task cgvbrowser.Task) (scheduleBrowser, error) {
			return factory.Open(ctx, task)
		},
		clock: time.Now,
	}, nil
}

func (executor *CGVExecutor) Capture(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) ([]*observationpb.Capture, error) {
	return executor.captureSchedules(ctx, task, nil)
}

// CaptureWeekdays keeps an active monitor scan inside CGV's currently visible
// range while limiting the expensive date traversal to relevant weekdays.
func (executor *CGVExecutor) CaptureWeekdays(
	ctx context.Context,
	task *observationpb.AssignmentTask,
	weekdayValues []int32,
) ([]*observationpb.Capture, error) {
	weekdays, err := scheduleWeekdays(weekdayValues)
	if err != nil {
		return nil, err
	}
	return executor.captureSchedules(ctx, task, weekdays)
}

func (executor *CGVExecutor) OpenScheduleSession(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) (*ScheduleSession, error) {
	if err := validateScheduleTask(task); err != nil {
		return nil, err
	}
	if executor == nil || executor.open == nil {
		return nil, fmt.Errorf("%w: probe browser factory is unavailable", cgv.ErrBrowserStartFailed)
	}
	browserSession, err := executor.open(ctx, scanBrowserTask(task))
	if err != nil {
		return nil, fmt.Errorf("%w: open Probe browser: %w", cgv.ErrBrowserStartFailed, err)
	}
	return &ScheduleSession{executor: executor, browser: browserSession}, nil
}

func (session *ScheduleSession) Capture(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) ([]*observationpb.Capture, error) {
	return session.capture(ctx, task, nil)
}

func (session *ScheduleSession) CaptureWeekdays(
	ctx context.Context,
	task *observationpb.AssignmentTask,
	weekdayValues []int32,
) ([]*observationpb.Capture, error) {
	weekdays, err := scheduleWeekdays(weekdayValues)
	if err != nil {
		return nil, err
	}
	return session.capture(ctx, task, weekdays)
}

func (session *ScheduleSession) CaptureWeekdayShard(
	ctx context.Context,
	task *observationpb.AssignmentTask,
	weekdayValues []int32,
	shard int,
) ([]*observationpb.Capture, error) {
	weekdays, err := scheduleWeekdays(weekdayValues)
	if err != nil {
		return nil, err
	}
	if session == nil || session.executor == nil {
		return nil, errors.New("probe schedule session is closed")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.browser == nil {
		return nil, errors.New("probe schedule session is closed")
	}
	return session.executor.captureScheduleWeekdayShardInBrowser(ctx, task, weekdays, shard, session.browser)
}

func (executor *CGVExecutor) captureScheduleWeekdayShardInBrowser(
	ctx context.Context,
	task *observationpb.AssignmentTask,
	weekdays []time.Weekday,
	shard int,
	browserSession scheduleBrowser,
) ([]*observationpb.Capture, error) {
	if err := validateScheduleTask(task); err != nil {
		return nil, err
	}
	shardBrowser, supported := browserSession.(weekdayShardScheduleBrowser)
	if !supported {
		return executor.captureSchedulesInBrowser(ctx, task, weekdays, browserSession)
	}
	schedule := task.GetSchedule()
	location, err := time.LoadLocation(schedule.GetTimeZone())
	if err != nil {
		return nil, fmt.Errorf("%w: load assignment time zone: %w", errLocalExecution, err)
	}
	values, err := shardBrowser.CaptureScheduleWeekdayShard(ctx, scheduleTheater(schedule.GetTheater()), weekdays, shard)
	if err != nil {
		return nil, fmt.Errorf("capture CGV schedule shard: %w", err)
	}
	result := make([]*observationpb.Capture, 0, len(values))
	for _, value := range values {
		capture, err := executor.convertCapture(value, location)
		if err != nil {
			return nil, err
		}
		result = append(result, capture)
	}
	return result, nil
}

func (session *ScheduleSession) capture(
	ctx context.Context,
	task *observationpb.AssignmentTask,
	weekdays []time.Weekday,
) ([]*observationpb.Capture, error) {
	if session == nil || session.executor == nil {
		return nil, errors.New("probe schedule session is closed")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.browser == nil {
		return nil, errors.New("probe schedule session is closed")
	}
	return session.executor.captureSchedulesInBrowser(ctx, task, weekdays, session.browser)
}

func (session *ScheduleSession) Close() {
	if session == nil {
		return
	}
	session.mu.Lock()
	browserSession := session.browser
	session.browser = nil
	session.mu.Unlock()
	if browserSession != nil {
		browserSession.Close()
	}
}

func (executor *CGVExecutor) captureSchedules(
	ctx context.Context,
	task *observationpb.AssignmentTask,
	weekdays []time.Weekday,
) ([]*observationpb.Capture, error) {
	if err := validateScheduleTask(task); err != nil {
		return nil, err
	}
	browserSession, err := executor.open(ctx, scanBrowserTask(task))
	if err != nil {
		return nil, fmt.Errorf("%w: open Probe browser: %w", cgv.ErrBrowserStartFailed, err)
	}
	defer browserSession.Close()
	return executor.captureSchedulesInBrowser(ctx, task, weekdays, browserSession)
}

func (executor *CGVExecutor) captureSchedulesInBrowser(
	ctx context.Context,
	task *observationpb.AssignmentTask,
	weekdays []time.Weekday,
	browserSession scheduleBrowser,
) ([]*observationpb.Capture, error) {
	if err := validateScheduleTask(task); err != nil {
		return nil, err
	}
	schedule := task.GetSchedule()
	location, err := time.LoadLocation(schedule.GetTimeZone())
	if err != nil {
		return nil, fmt.Errorf("%w: load assignment time zone: %w", errLocalExecution, err)
	}
	theater := scheduleTheater(schedule.GetTheater())
	var values []cgv.ScheduleCapture
	if weekdayBrowser, supported := browserSession.(weekdayScheduleBrowser); supported && len(weekdays) > 0 {
		values, err = weekdayBrowser.CaptureSchedulesForWeekdays(ctx, theater, weekdays)
	} else {
		values, err = browserSession.CaptureSchedules(ctx, theater, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("capture CGV schedules: %w", err)
	}
	result := make([]*observationpb.Capture, 0, len(values))
	for _, value := range values {
		capture, err := executor.convertCapture(value, location)
		if err != nil {
			return nil, err
		}
		result = append(result, capture)
	}
	return result, nil
}

func scheduleWeekdays(values []int32) ([]time.Weekday, error) {
	seen := make(map[time.Weekday]struct{}, len(values))
	result := make([]time.Weekday, 0, len(values))
	for _, value := range values {
		if value < int32(time.Sunday) || value > int32(time.Saturday) {
			return nil, fmt.Errorf("%w: invalid schedule weekday %d", errLocalExecution, value)
		}
		weekday := time.Weekday(value)
		if _, exists := seen[weekday]; exists {
			continue
		}
		seen[weekday] = struct{}{}
		result = append(result, weekday)
	}
	return result, nil
}

func (executor *CGVExecutor) CaptureCatalog(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) (*catalogpb.CatalogSnapshot, error) {
	catalogTask := task.GetCatalog()
	if catalogTask == nil {
		return nil, fmt.Errorf("%w: unsupported Probe task kind", errLocalExecution)
	}
	if catalogTask.GetProviderId() != cgv.ProviderCGV {
		return nil, fmt.Errorf("%w: unsupported catalog provider", errLocalExecution)
	}
	if err := requireManagedScan(task); err != nil {
		return nil, err
	}
	browserSession, err := executor.open(ctx, scanBrowserTask(task))
	if err != nil {
		return nil, fmt.Errorf("%w: open Probe browser: %w", cgv.ErrBrowserStartFailed, err)
	}
	defer browserSession.Close()
	catalog, supported := browserSession.(catalogBrowser)
	if !supported {
		return nil, fmt.Errorf("%w: probe browser does not support catalog capture", errLocalExecution)
	}
	value, err := catalog.CaptureCatalog(ctx, catalogTask.GetCachedPosterMovieIds())
	if err != nil {
		return nil, fmt.Errorf("capture CGV catalog: %w", err)
	}
	snapshot := &catalogpb.CatalogSnapshot{}
	provider := &catalogpb.Provider{}
	provider.SetId(cgv.ProviderCGV)
	provider.SetName("CGV")
	snapshot.SetProvider(provider)
	snapshot.SetTheaters(make([]*catalogpb.Theater, 0, len(value.Theaters)))
	snapshot.SetMovies(make([]*catalogpb.Movie, 0, len(value.Movies)))
	snapshot.SetObservedAt(timestamppb.New(executor.clock().UTC()))
	for _, theater := range value.Theaters {
		item := &catalogpb.Theater{}
		item.SetId(cgv.CatalogID(cgv.ProviderCGV, "theater", theater.SourceKey))
		item.SetProviderId(cgv.ProviderCGV)
		item.SetIdentity(cgv.NewTheaterIdentity(theater.SourceKey))
		item.SetRegion(theater.Region)
		item.SetName(theater.Name)
		snapshot.SetTheaters(append(snapshot.GetTheaters(), item))
	}
	for _, movie := range value.Movies {
		item := &catalogpb.Movie{}
		item.SetId(cgv.CatalogID(cgv.ProviderCGV, "movie", movie.SourceKey))
		item.SetProviderId(cgv.ProviderCGV)
		item.SetIdentity(cgv.NewMovieIdentity(movie.SourceKey))
		item.SetTitle(movie.Title)
		snapshot.SetMovies(append(snapshot.GetMovies(), item))
	}
	posters := make([]*catalogpb.MoviePoster, 0, len(value.Posters))
	for _, poster := range value.Posters {
		movieID := cgv.CatalogID(cgv.ProviderCGV, "movie", poster.MovieSourceKey)
		posters = append(posters, catalogpb.MoviePoster_builder{
			MovieId: &movieID, MediaType: &poster.MediaType, Data: poster.Data, ContentHash: &poster.ContentHash,
		}.Build())
	}
	snapshot.SetPosters(posters)
	return snapshot, nil
}

func validateScheduleTask(task *observationpb.AssignmentTask) error {
	schedule := task.GetSchedule()
	if schedule == nil {
		return fmt.Errorf("%w: unsupported Probe task kind", errLocalExecution)
	}
	if err := requireManagedScan(task); err != nil {
		return err
	}
	theater := schedule.GetTheater()
	siteNo, validIdentity := cgv.TheaterSiteNo(theater)
	if theater == nil || theater.GetProviderId() != cgv.ProviderCGV || !validIdentity ||
		strings.TrimSpace(theater.GetRegion()) == "" || strings.TrimSpace(theater.GetName()) == "" {
		return fmt.Errorf("%w: CGV schedule task theater identity is incomplete", cgv.ErrIdentityMismatch)
	}
	if theater.GetId() != cgv.CatalogID(cgv.ProviderCGV, "theater", siteNo) {
		return fmt.Errorf("%w: CGV schedule task theater ID is not canonical", cgv.ErrIdentityMismatch)
	}
	if strings.TrimSpace(schedule.GetTimeZone()) == "" {
		return fmt.Errorf("%w: CGV schedule task time zone is incomplete", cgv.ErrIdentityMismatch)
	}
	return nil
}

func requireManagedScan(task *observationpb.AssignmentTask) error {
	if task == nil || task.GetEgress() == nil || task.GetEgress().GetManagedScan() == nil {
		return fmt.Errorf("%w: assignment requires the managed scan egress policy", errLocalExecution)
	}
	return nil
}

func scheduleTheater(theater *catalogpb.Theater) cgv.ScheduleTheater {
	if theater == nil {
		return cgv.ScheduleTheater{}
	}
	return cgv.ScheduleTheater{
		ID: theater.GetId(), ProviderID: theater.GetProviderId(), SourceKey: siteNoForTheater(theater),
		Region: theater.GetRegion(), Name: theater.GetName(),
	}
}

func siteNoForTheater(theater *catalogpb.Theater) string {
	value, _ := cgv.TheaterSiteNo(theater)
	return value
}

func scanBrowserTask(task *observationpb.AssignmentTask) cgvbrowser.Task {
	result := cgvbrowser.Task{Purpose: egress.PurposeScan, EgressPolicyID: egress.PolicyScanDefault, Headless: true}
	switch {
	case task != nil && task.GetSchedule() != nil:
		result.Locale = task.GetSchedule().GetLocale()
		result.TimeZone = task.GetSchedule().GetTimeZone()
	case task != nil && task.GetCatalog() != nil:
		result.Locale = task.GetCatalog().GetLocale()
		result.TimeZone = task.GetCatalog().GetTimeZone()
	case task != nil && task.GetSeatMap() != nil:
		result.Locale = task.GetSeatMap().GetLocale()
		result.TimeZone = task.GetSeatMap().GetTimeZone()
	}
	return result
}

func (executor *CGVExecutor) convertCapture(
	value cgv.ScheduleCapture,
	location *time.Location,
) (*observationpb.Capture, error) {
	targetDate, err := localDate(value.TargetDate)
	if err != nil {
		return nil, err
	}
	observedAt := executor.clock().UTC()
	capture := &observationpb.Capture{}
	capture.SetTargetDate(targetDate)
	capture.SetComplete(value.Complete)
	if !value.Complete {
		capture.SetErrorCode(captureErrorCode(value.Error))
	}
	showtimes := make([]*catalogpb.Showtime, 0, len(value.Showtimes))
	for _, showtime := range value.Showtimes {
		showtimeParts, showtimeIdentity, err := canonicalShowtimeContract(showtime)
		if err != nil {
			return nil, err
		}
		startsAt, endsAt, err := cgv.ParseShowtimeRange(showtime.Date, showtime.StartsAt, showtime.EndsAt, location)
		if err != nil {
			return nil, fmt.Errorf("convert showtime %q: %w", showtime.ID, err)
		}
		if showtime.ObservedAt.After(observedAt) {
			observedAt = showtime.ObservedAt.UTC()
		}
		capacity, err := int32Value(showtime.Capacity)
		if err != nil {
			return nil, fmt.Errorf("convert showtime %q capacity: %w", showtime.ID, err)
		}
		availableSeats, err := int32Value(showtime.AvailableSeats)
		if err != nil {
			return nil, fmt.Errorf("convert showtime %q available seats: %w", showtime.ID, err)
		}
		movie := &catalogpb.Movie{}
		movie.SetId(showtime.MovieID)
		movie.SetProviderId(showtime.ProviderID)
		movie.SetIdentity(cgv.NewMovieIdentity(showtime.MovieSourceKey))
		movie.SetTitle(showtime.MovieTitle)
		auditorium := &catalogpb.Auditorium{}
		auditorium.SetId(showtime.AuditoriumID)
		auditorium.SetTheaterId(showtime.TheaterID)
		auditorium.SetIdentity(cgv.NewAuditoriumIdentity(showtimeParts[0], showtimeParts[2]))
		auditorium.SetName(showtime.AuditoriumName)
		auditorium.SetScreenTypes(append([]string(nil), showtime.ScreenTypes...))
		auditorium.SetCapacity(capacity)
		item := &catalogpb.Showtime{}
		item.SetId(showtime.ID)
		item.SetProviderId(showtime.ProviderID)
		item.SetTheaterId(showtime.TheaterID)
		item.SetMovie(movie)
		item.SetAuditorium(auditorium)
		item.SetIdentity(showtimeIdentity)
		item.SetStartsAt(timestamppb.New(startsAt))
		item.SetEndsAt(timestamppb.New(endsAt))
		item.SetAvailableSeats(availableSeats)
		item.SetCapacity(capacity)
		item.SetSoldOut(showtime.SoldOut)
		showtimes = append(showtimes, item)
	}
	capture.SetObservedAt(timestamppb.New(observedAt))
	capture.SetShowtimes(showtimes)
	return capture, nil
}

func validateCanonicalShowtime(showtime cgv.ScheduleShowtime) error {
	_, _, err := canonicalShowtimeContract(showtime)
	return err
}

func canonicalShowtimeContract(showtime cgv.ScheduleShowtime) ([]string, *catalogpb.ShowtimeIdentity, error) {
	if showtime.ProviderID != cgv.ProviderCGV || strings.TrimSpace(showtime.TheaterID) == "" {
		return nil, nil, fmt.Errorf("%w: convert showtime provider and theater identities are required", cgv.ErrIdentityMismatch)
	}
	showtimeParts, err := canonicalShowtimeSourceParts(showtime.SourceKey)
	if err != nil {
		return nil, nil, err
	}
	if err := validateAuditoriumSourceKey(showtime.AuditoriumSourceKey, showtimeParts[0], showtimeParts[2]); err != nil {
		return nil, nil, err
	}
	identities := []struct {
		kind      string
		id        string
		sourceKey string
	}{
		{kind: "showtime", id: showtime.ID, sourceKey: showtime.SourceKey},
		{kind: "movie", id: showtime.MovieID, sourceKey: showtime.MovieSourceKey},
		{kind: "auditorium", id: showtime.AuditoriumID, sourceKey: showtime.AuditoriumSourceKey},
	}
	for _, identity := range identities {
		if strings.TrimSpace(identity.sourceKey) == "" ||
			identity.id != cgv.CatalogID(cgv.ProviderCGV, identity.kind, identity.sourceKey) {
			return nil, nil, fmt.Errorf("%w: convert showtime %s identity is not canonical", cgv.ErrIdentityMismatch, identity.kind)
		}
	}
	if showtime.TheaterID != cgv.CatalogID(cgv.ProviderCGV, "theater", showtimeParts[0]) {
		return nil, nil, fmt.Errorf("%w: convert theater identity is not canonical", cgv.ErrIdentityMismatch)
	}
	// canonicalShowtimeSourceParts already proved that the schedule date is valid.
	showtimeIdentity, _ := cgv.NewShowtimeIdentity(showtimeParts[0], showtimeParts[1], showtimeParts[2], showtimeParts[3])
	return showtimeParts, showtimeIdentity, nil
}

func canonicalShowtimeSourceParts(sourceKey string) ([]string, error) {
	parts := strings.Split(sourceKey, "/")
	if len(parts) != 4 || !numericIdentifier(parts[0]) || !numericIdentifier(parts[2]) || !numericIdentifier(parts[3]) {
		return nil, fmt.Errorf("%w: convert showtime source tuple is not canonical", cgv.ErrIdentityMismatch)
	}
	parsedDate, err := time.Parse(time.DateOnly, parts[1])
	if err != nil || parsedDate.Format(time.DateOnly) != parts[1] {
		return nil, fmt.Errorf("%w: convert showtime source date is not canonical", cgv.ErrIdentityMismatch)
	}
	return parts, nil
}

func validateAuditoriumSourceKey(sourceKey, siteNo, screenNo string) error {
	parts := strings.Split(sourceKey, "/")
	if len(parts) != 2 || parts[0] != siteNo || parts[1] != screenNo ||
		!numericIdentifier(parts[0]) || !numericIdentifier(parts[1]) {
		return fmt.Errorf("%w: convert auditorium source tuple is not canonical", cgv.ErrIdentityMismatch)
	}
	return nil
}

func numericIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func localDate(value string) (*commonpb.LocalDate, error) {
	parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("invalid local date %q: %w", value, err)
	}
	// DateOnly parsing constrains these calendar components to int32-safe values.
	year, _ := int32Value(parsed.Year())
	month, _ := int32Value(int(parsed.Month()))
	day, _ := int32Value(parsed.Day())
	result := &commonpb.LocalDate{}
	result.SetYear(year)
	result.SetMonth(month)
	result.SetDay(day)
	return result, nil
}

func int32Value(value int) (int32, error) {
	if value < math.MinInt32 || value > math.MaxInt32 {
		return 0, fmt.Errorf("integer %d is outside int32 range", value)
	}
	return int32(value), nil
}

func captureErrorCode(value string) string {
	switch {
	case strings.TrimSpace(value) == "":
		return "capture_incomplete"
	case strings.Contains(value, cgv.ErrUIContractChanged.Error()):
		return "ui_contract_changed"
	case strings.Contains(value, context.DeadlineExceeded.Error()):
		return "capture_timeout"
	default:
		return "capture_failed"
	}
}
