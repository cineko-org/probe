package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	central "github.com/cineko-org/contracts/v3"
	"github.com/cineko-org/probe/v2/internal/egress"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	cgvbrowser "github.com/cineko-org/probe/v2/internal/provider/cgv/browser"
)

type scheduleBrowser interface {
	CaptureSchedules(context.Context, cgv.ScheduleTheater, []string) ([]cgv.ScheduleCapture, error)
	Close()
}

type catalogBrowser interface {
	CaptureCatalog(context.Context) (cgv.CatalogCapture, error)
}

type seatMapBrowser interface {
	CaptureSeatMap(context.Context, central.AssignmentTask) (*central.SeatMapVersion, error)
}

type CGVExecutor struct {
	open    func(context.Context, cgvbrowser.Task) (scheduleBrowser, error)
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
	if executor.seatMap != nil {
		return executor.seatMap.CaptureSeatMap(ctx, task)
	}
	if executor.open == nil {
		return nil, errors.New("probe browser factory is unavailable")
	}
	browserSession, err := executor.open(ctx, cgvbrowser.Task{
		Purpose: egress.PurposeScan, EgressPolicyID: task.EgressPolicyID, Headless: true,
		Locale: task.Locale, TimeZone: task.TimeZone,
	})
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
	task central.AssignmentTask,
) ([]central.Capture, error) {
	if err := validateScheduleTask(task); err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(task.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("load assignment time zone: %w", err)
	}
	browserSession, err := executor.open(ctx, cgvbrowser.Task{
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
	browserSession, err := executor.open(ctx, cgvbrowser.Task{
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
		startsAt, endsAt, err := cgv.ParseShowtimeRange(showtime.Date, showtime.StartsAt, showtime.EndsAt, location)
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
