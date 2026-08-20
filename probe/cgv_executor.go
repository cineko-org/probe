package probe

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	central "github.com/cineko-org/contracts/v3"
	"github.com/cineko-org/probe/v2/internal/adapters/cgv"
	browserruntime "github.com/cineko-org/probe/v2/internal/browser"
	"github.com/cineko-org/probe/v2/internal/egress"
)

type scheduleBrowser interface {
	CaptureSchedules(context.Context, cgv.ScheduleTheater, []string) ([]cgv.ScheduleCapture, error)
	Close()
}

type catalogBrowser interface {
	CaptureCatalog(context.Context) (cgv.CatalogCapture, error)
}

type CGVExecutor struct {
	open    func(context.Context, browserruntime.Task) (scheduleBrowser, error)
	clock   func() time.Time
	seatMap SeatMapExecutor
}

func (executor *CGVExecutor) CaptureSeatMap(
	ctx context.Context,
	task central.AssignmentTask,
) (*central.SeatMapVersion, error) {
	if task.Kind != central.CapabilityCGVSeatMapCapture {
		return nil, fmt.Errorf("unsupported Probe task kind %q", task.Kind)
	}
	if executor.seatMap == nil {
		return nil, errors.New("probe executor does not support seat-map capture")
	}
	return executor.seatMap.CaptureSeatMap(ctx, task)
}

func NewCGVExecutor(factory *browserruntime.Factory) (*CGVExecutor, error) {
	if factory == nil {
		return nil, errors.New("probe browser factory is required")
	}
	return &CGVExecutor{
		open: func(ctx context.Context, task browserruntime.Task) (scheduleBrowser, error) {
			return factory.Open(ctx, task)
		},
		clock: time.Now,
	}, nil
}

func (executor *CGVExecutor) Capture(
	ctx context.Context,
	task central.AssignmentTask,
) ([]central.Capture, error) {
	if err := validateScheduleTask(task); err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(task.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("load assignment time zone: %w", err)
	}
	browserSession, err := executor.open(ctx, browserruntime.Task{
		Purpose: egress.PurposeScan, EgressPolicyID: task.EgressPolicyID, Headless: true,
		Locale: task.Locale, TimeZone: task.TimeZone,
	})
	if err != nil {
		return nil, fmt.Errorf("open Probe browser: %w", err)
	}
	defer browserSession.Close()
	theater := cgv.ScheduleTheater{
		ID: task.Theater.ID, ProviderID: task.Theater.ProviderID, SourceKey: task.Theater.SourceKey,
		Region: task.Theater.Region, Name: task.Theater.Name,
	}
	values, err := browserSession.CaptureSchedules(ctx, theater, task.TargetDates)
	if err != nil {
		return nil, fmt.Errorf("capture CGV schedules: %w", err)
	}
	result := make([]central.Capture, 0, len(values))
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
	task central.AssignmentTask,
) (*central.CatalogSnapshot, error) {
	if task.Kind != central.CapabilityCGVCatalogCapture {
		return nil, fmt.Errorf("unsupported Probe task kind %q", task.Kind)
	}
	browserSession, err := executor.open(ctx, browserruntime.Task{
		Purpose: egress.PurposeScan, EgressPolicyID: task.EgressPolicyID, Headless: true,
		Locale: task.Locale, TimeZone: task.TimeZone,
	})
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
	snapshot := &central.CatalogSnapshot{
		Provider:   central.Provider{ID: central.ProviderCGV, Name: "CGV"},
		Theaters:   make([]central.Theater, 0, len(value.Theaters)),
		Movies:     make([]central.Movie, 0, len(value.Movies)),
		ObservedAt: executor.clock().UTC(),
	}
	for _, theater := range value.Theaters {
		snapshot.Theaters = append(snapshot.Theaters, central.Theater{
			ID:         central.CatalogID(central.ProviderCGV, "theater", theater.SourceKey),
			ProviderID: central.ProviderCGV, SourceKey: theater.SourceKey,
			Region: theater.Region, Name: theater.Name,
		})
	}
	for _, movie := range value.Movies {
		snapshot.Movies = append(snapshot.Movies, central.Movie{
			ID:         central.CatalogID(central.ProviderCGV, "movie", movie.SourceKey),
			ProviderID: central.ProviderCGV, SourceKey: movie.SourceKey,
			Title: movie.Title, PosterURL: movie.PosterURL,
		})
	}
	return snapshot, nil
}

func validateScheduleTask(task central.AssignmentTask) error {
	if task.Kind != central.CapabilityCGVScheduleCapture {
		return fmt.Errorf("unsupported Probe task kind %q", task.Kind)
	}
	theater := task.Theater
	if theater.ProviderID != central.ProviderCGV || strings.TrimSpace(theater.SourceKey) == "" ||
		strings.TrimSpace(theater.Region) == "" || strings.TrimSpace(theater.Name) == "" {
		return errors.New("CGV schedule task theater identity is incomplete")
	}
	if theater.ID != central.CatalogID(central.ProviderCGV, "theater", theater.SourceKey) {
		return errors.New("CGV schedule task theater ID is not canonical")
	}
	return nil
}

func (executor *CGVExecutor) convertCapture(
	value cgv.ScheduleCapture,
	location *time.Location,
) (central.Capture, error) {
	capture := central.Capture{
		TargetDate: value.TargetDate, Complete: value.Complete, ObservedAt: executor.clock().UTC(),
		Showtimes: make([]central.Showtime, 0, len(value.Showtimes)),
	}
	if !value.Complete {
		capture.ErrorCode = captureErrorCode(value.Error)
	}
	for _, showtime := range value.Showtimes {
		if err := validateCanonicalShowtime(showtime); err != nil {
			return central.Capture{}, err
		}
		startsAt, endsAt, err := showtimeRange(showtime.Date, showtime.StartsAt, showtime.EndsAt, location)
		if err != nil {
			return central.Capture{}, fmt.Errorf("convert showtime %q: %w", showtime.ID, err)
		}
		if showtime.ObservedAt.After(capture.ObservedAt) {
			capture.ObservedAt = showtime.ObservedAt.UTC()
		}
		capture.Showtimes = append(capture.Showtimes, central.Showtime{
			ID: showtime.ID, ProviderID: showtime.ProviderID,
			SourceKey: showtime.SourceKey, TheaterID: showtime.TheaterID,
			Movie: central.Movie{
				ID: showtime.MovieID, ProviderID: showtime.ProviderID,
				SourceKey: showtime.MovieSourceKey, Title: showtime.MovieTitle, PosterURL: showtime.PosterURL,
			},
			Auditorium: central.Auditorium{
				ID: showtime.AuditoriumID, TheaterID: showtime.TheaterID,
				SourceKey: showtime.AuditoriumSourceKey, Name: showtime.AuditoriumName,
				ScreenTypes: append([]string(nil), showtime.ScreenTypes...), Capacity: showtime.Capacity,
			},
			StartsAt: startsAt, EndsAt: endsAt, AvailableSeats: showtime.AvailableSeats,
			Capacity: showtime.Capacity, SoldOut: showtime.SoldOut,
		})
	}
	return capture, nil
}

func validateCanonicalShowtime(showtime cgv.ScheduleShowtime) error {
	if showtime.ProviderID != central.ProviderCGV || strings.TrimSpace(showtime.TheaterID) == "" {
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
			identity.id != central.CatalogID(central.ProviderCGV, identity.kind, identity.sourceKey) {
			return fmt.Errorf("convert showtime: %s identity is not canonical", identity.kind)
		}
	}
	return nil
}

func showtimeRange(
	date string,
	startClock string,
	endClock string,
	location *time.Location,
) (time.Time, time.Time, error) {
	if location == nil {
		return time.Time{}, time.Time{}, errors.New("showtime location is required")
	}
	day, err := time.ParseInLocation(time.DateOnly, strings.TrimSpace(date), location)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	startHour, startMinute, err := parseCGVClock(startClock)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse start clock: %w", err)
	}
	endHour, endMinute, err := parseCGVClock(endClock)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse end clock: %w", err)
	}
	startsAt := day.Add(time.Duration(startHour)*time.Hour + time.Duration(startMinute)*time.Minute)
	endsAt := day.Add(time.Duration(endHour)*time.Hour + time.Duration(endMinute)*time.Minute)
	if endsAt.Equal(startsAt) {
		return time.Time{}, time.Time{}, errors.New("showtime end must differ from start")
	}
	if endsAt.Before(startsAt) {
		endsAt = endsAt.Add(24 * time.Hour)
	}
	const maxShowtimeDuration = 24 * time.Hour
	if !endsAt.After(startsAt) || endsAt.Sub(startsAt) > maxShowtimeDuration {
		return time.Time{}, time.Time{}, errors.New("showtime duration is unreasonable")
	}
	return startsAt, endsAt, nil
}

// parseCGVClock accepts CGV's HH:MM schedule clock. CGV uses extended-hour
// values such as 24:30 for a show on the following calendar day while keeping
// the requested scnYmd. Keeping the hour offset here lets the caller derive
// the real KST weekday without changing the provider identity tuple.
func parseCGVClock(value string) (int, int, error) {
	value = strings.TrimSpace(value)
	if len(value) != 5 || value[2] != ':' {
		return 0, 0, fmt.Errorf("invalid clock %q", value)
	}
	hour, err := strconv.Atoi(value[:2])
	if err != nil || hour < 0 || hour > 47 {
		return 0, 0, fmt.Errorf("invalid hour in %q", value)
	}
	minute, err := strconv.Atoi(value[3:])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q", value)
	}
	return hour, minute, nil
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
