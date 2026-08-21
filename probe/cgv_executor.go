package probe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	catalogpb "github.com/cineko-org/contracts/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/gen/go/cineko/observation"
	seatmappb "github.com/cineko-org/contracts/gen/go/cineko/seatmap"
	"github.com/cineko-org/probe/v2/internal/egress"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	cgvbrowser "github.com/cineko-org/probe/v2/internal/provider/cgv/browser"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type scheduleBrowser interface {
	CaptureSchedules(context.Context, cgv.ScheduleTheater, []string) ([]cgv.ScheduleCapture, error)
	Close()
}

type catalogBrowser interface {
	CaptureCatalog(context.Context) (cgv.CatalogCapture, error)
}

type seatMapBrowser interface {
	CaptureSeatMap(context.Context, *observationpb.AssignmentTask) (*seatmappb.Snapshot, error)
}

type CGVExecutor struct {
	open    func(context.Context, cgvbrowser.Task) (scheduleBrowser, error)
	clock   func() time.Time
	seatMap SeatMapExecutor
}

func (executor *CGVExecutor) CaptureSeatMap(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) (*seatmappb.Snapshot, error) {
	seatTask := task.GetSeatMap()
	if seatTask == nil {
		return nil, errors.New("unsupported Probe task kind")
	}
	if err := requireManagedScan(task); err != nil {
		return nil, err
	}
	if executor.seatMap != nil {
		return executor.seatMap.CaptureSeatMap(ctx, task)
	}
	if executor.open == nil {
		return nil, errors.New("probe browser factory is unavailable")
	}
	browserSession, err := executor.open(ctx, scanBrowserTask(task))
	if err != nil {
		return nil, fmt.Errorf("open Probe browser: %w", err)
	}
	defer browserSession.Close()
	capture, supported := browserSession.(seatMapBrowser)
	if !supported {
		return nil, errors.New("probe browser does not support seat-map capture")
	}
	return capture.CaptureSeatMap(ctx, task)
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
	schedule := task.GetSchedule()
	if schedule == nil {
		return nil, errors.New("unsupported Probe task kind")
	}
	if err := validateScheduleTask(task); err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(schedule.GetTimeZone())
	if err != nil {
		return nil, fmt.Errorf("load assignment time zone: %w", err)
	}
	browserSession, err := executor.open(ctx, scanBrowserTask(task))
	if err != nil {
		return nil, fmt.Errorf("open Probe browser: %w", err)
	}
	defer browserSession.Close()
	theater := scheduleTheater(schedule.GetTheater())
	targetDates := make([]string, 0, len(schedule.GetTargetDates()))
	for _, date := range schedule.GetTargetDates() {
		targetDates = append(targetDates, localDateString(date))
	}
	values, err := browserSession.CaptureSchedules(ctx, theater, targetDates)
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

func (executor *CGVExecutor) CaptureCatalog(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) (*catalogpb.CatalogSnapshot, error) {
	catalogTask := task.GetCatalog()
	if catalogTask == nil {
		return nil, errors.New("unsupported Probe task kind")
	}
	if err := requireManagedScan(task); err != nil {
		return nil, err
	}
	browserSession, err := executor.open(ctx, scanBrowserTask(task))
	if err != nil {
		return nil, fmt.Errorf("open Probe browser: %w", err)
	}
	defer browserSession.Close()
	catalog, supported := browserSession.(catalogBrowser)
	if !supported {
		return nil, errors.New("probe browser does not support catalog capture")
	}
	value, err := catalog.CaptureCatalog(ctx)
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
		item.SetSourceKey(theater.SourceKey)
		item.SetRegion(theater.Region)
		item.SetName(theater.Name)
		snapshot.SetTheaters(append(snapshot.GetTheaters(), item))
	}
	for _, movie := range value.Movies {
		item := &catalogpb.Movie{}
		item.SetId(cgv.CatalogID(cgv.ProviderCGV, "movie", movie.SourceKey))
		item.SetProviderId(cgv.ProviderCGV)
		item.SetSourceKey(movie.SourceKey)
		item.SetTitle(movie.Title)
		item.SetPosterUrl(movie.PosterURL)
		snapshot.SetMovies(append(snapshot.GetMovies(), item))
	}
	return snapshot, nil
}

func validateScheduleTask(task *observationpb.AssignmentTask) error {
	schedule := task.GetSchedule()
	if schedule == nil {
		return errors.New("unsupported Probe task kind")
	}
	if err := requireManagedScan(task); err != nil {
		return err
	}
	theater := schedule.GetTheater()
	if theater == nil || theater.GetProviderId() != cgv.ProviderCGV || strings.TrimSpace(theater.GetSourceKey()) == "" ||
		strings.TrimSpace(theater.GetRegion()) == "" || strings.TrimSpace(theater.GetName()) == "" {
		return errors.New("CGV schedule task theater identity is incomplete")
	}
	if theater.GetId() != cgv.CatalogID(cgv.ProviderCGV, "theater", theater.GetSourceKey()) {
		return errors.New("CGV schedule task theater ID is not canonical")
	}
	if strings.TrimSpace(schedule.GetTimeZone()) == "" || len(schedule.GetTargetDates()) == 0 {
		return errors.New("CGV schedule task capture window is incomplete")
	}
	return nil
}

func requireManagedScan(task *observationpb.AssignmentTask) error {
	if task == nil || task.GetEgress() == nil || task.GetEgress().GetManagedScan() == nil {
		return errors.New("assignment requires the managed scan egress policy")
	}
	return nil
}

func scheduleTheater(theater *catalogpb.Theater) cgv.ScheduleTheater {
	if theater == nil {
		return cgv.ScheduleTheater{}
	}
	return cgv.ScheduleTheater{
		ID: theater.GetId(), ProviderID: theater.GetProviderId(), SourceKey: theater.GetSourceKey(),
		Region: theater.GetRegion(), Name: theater.GetName(),
	}
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
		if err := validateCanonicalShowtime(showtime); err != nil {
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
		movie.SetSourceKey(showtime.MovieSourceKey)
		movie.SetTitle(showtime.MovieTitle)
		movie.SetPosterUrl(showtime.PosterURL)
		auditorium := &catalogpb.Auditorium{}
		auditorium.SetId(showtime.AuditoriumID)
		auditorium.SetTheaterId(showtime.TheaterID)
		auditorium.SetSourceKey(showtime.AuditoriumSourceKey)
		auditorium.SetName(showtime.AuditoriumName)
		auditorium.SetScreenTypes(append([]string(nil), showtime.ScreenTypes...))
		auditorium.SetCapacity(capacity)
		item := &catalogpb.Showtime{}
		item.SetId(showtime.ID)
		item.SetProviderId(showtime.ProviderID)
		item.SetSourceKey(showtime.SourceKey)
		item.SetTheaterId(showtime.TheaterID)
		item.SetMovie(movie)
		item.SetAuditorium(auditorium)
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
	if showtime.ProviderID != cgv.ProviderCGV || strings.TrimSpace(showtime.TheaterID) == "" {
		return errors.New("convert showtime: provider and theater identities are required")
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
			return fmt.Errorf("convert showtime: %s identity is not canonical", identity.kind)
		}
	}
	return nil
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

func localDateString(value *commonpb.LocalDate) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", value.GetYear(), value.GetMonth(), value.GetDay())
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
