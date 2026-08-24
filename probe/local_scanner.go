package probe

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	seatmappb "github.com/cineko-org/contracts/v3/gen/go/cineko/seatmap"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	cgvbrowser "github.com/cineko-org/probe/v2/internal/provider/cgv/browser"
)

// LocalScanner is the in-process scanner owned by Client. It performs anonymous
// catalog, schedule, and seat-map reads without any remote coordination.
type LocalScanner struct {
	factory  *cgvbrowser.Factory
	executor *CGVExecutor
	logger   *slog.Logger

	scheduleContext context.Context
	cancelSchedule  context.CancelFunc
	scheduleMu      sync.Mutex
	scheduleSession *ScheduleSession
	closed          bool
}

type LocalScannerConfig struct {
	DataDir string
	Logger  *slog.Logger
}

func NewLocalScanner(config LocalScannerConfig) (*LocalScanner, error) {
	if strings.TrimSpace(config.DataDir) == "" {
		return nil, errors.New("local scanner data directory is required")
	}
	factory, err := cgvbrowser.NewFromEnvironmentWithLogger(filepath.Clean(config.DataDir), config.Logger)
	if err != nil {
		return nil, err
	}
	executor, err := NewCGVExecutor(factory)
	if err != nil {
		factory.Close()
		return nil, err
	}
	scheduleContext, cancelSchedule := context.WithCancel(context.Background())
	return &LocalScanner{
		factory: factory, executor: executor, logger: config.Logger,
		scheduleContext: scheduleContext, cancelSchedule: cancelSchedule,
	}, nil
}

func (scanner *LocalScanner) Close() error {
	if scanner == nil {
		return nil
	}
	scanner.scheduleMu.Lock()
	if scanner.closed {
		scanner.scheduleMu.Unlock()
		return nil
	}
	scanner.closed = true
	if scanner.cancelSchedule != nil {
		scanner.cancelSchedule()
	}
	scheduleSession := scanner.scheduleSession
	scanner.scheduleSession = nil
	scanner.scheduleMu.Unlock()
	if scheduleSession != nil {
		scheduleSession.Close()
	}
	if scanner.factory != nil {
		scanner.factory.Close()
	}
	return nil
}

func (scanner *LocalScanner) CaptureCatalog(
	ctx context.Context,
	cachedPosterMovieIDs []string,
) (*catalogpb.CatalogSnapshot, error) {
	if scanner == nil || scanner.executor == nil {
		return nil, errors.New("local scanner is closed")
	}
	providerID, locale, timeZone := cgv.ProviderCGV, "ko-KR", "Asia/Seoul"
	task := &observationpb.AssignmentTask{}
	task.SetEgress(managedScanEgress())
	task.SetCatalog(observationpb.CatalogTask_builder{
		ProviderId: &providerID, Locale: &locale, TimeZone: &timeZone,
		CachedPosterMovieIds: append([]string(nil), cachedPosterMovieIDs...),
	}.Build())
	return scanner.executor.CaptureCatalog(ctx, task)
}

func (scanner *LocalScanner) CaptureSchedules(
	ctx context.Context,
	theater *catalogpb.Theater,
) ([]*observationpb.Capture, error) {
	return scanner.captureSchedules(ctx, theater, nil)
}

func (scanner *LocalScanner) CaptureScheduleWeekdays(
	ctx context.Context,
	theater *catalogpb.Theater,
	weekdays []int32,
) ([]*observationpb.Capture, error) {
	return scanner.captureSchedules(ctx, theater, weekdays)
}

func (scanner *LocalScanner) captureSchedules(
	ctx context.Context,
	theater *catalogpb.Theater,
	weekdays []int32,
) ([]*observationpb.Capture, error) {
	if scanner == nil || scanner.executor == nil {
		return nil, errors.New("local scanner is closed")
	}
	locale, timeZone := "ko-KR", "Asia/Seoul"
	task := &observationpb.AssignmentTask{}
	task.SetEgress(managedScanEgress())
	task.SetSchedule(observationpb.ScheduleTask_builder{
		Theater: theater, Locale: &locale, TimeZone: &timeZone,
	}.Build())
	startedAt := time.Now()
	scanner.scheduleMu.Lock()
	defer scanner.scheduleMu.Unlock()
	if scanner.closed {
		return nil, errors.New("local scanner is closed")
	}
	reused := scanner.scheduleSession != nil
	if scanner.scheduleSession == nil {
		session, err := scanner.executor.OpenScheduleSession(scanner.scheduleContext, task)
		if err != nil {
			return nil, err
		}
		scanner.scheduleSession = session
	}
	var (
		captures []*observationpb.Capture
		err      error
	)
	if len(weekdays) > 0 {
		captures, err = scanner.scheduleSession.CaptureWeekdays(ctx, task, weekdays)
	} else {
		captures, err = scanner.scheduleSession.Capture(ctx, task)
	}
	if scanner.logger != nil {
		outcome := "succeeded"
		if err != nil {
			outcome = "failed"
		}
		scanner.logger.InfoContext(ctx, "Probe schedule browser round completed",
			"event", "scanner.schedule.session.used",
			"scenario", "schedule_collection",
			"operation", "capture_theater_schedule",
			"outcome", outcome,
			"browser_reused", reused,
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		scanner.scheduleSession.Close()
		scanner.scheduleSession = nil
	}
	return captures, err
}

func (scanner *LocalScanner) CaptureSeatMap(
	ctx context.Context,
	theater *catalogpb.Theater,
	auditorium *catalogpb.Auditorium,
) (*seatmappb.Snapshot, error) {
	if scanner == nil || scanner.executor == nil {
		return nil, errors.New("local scanner is closed")
	}
	locale, timeZone := "ko-KR", "Asia/Seoul"
	task := &observationpb.AssignmentTask{}
	task.SetEgress(managedScanEgress())
	task.SetSeatMap(observationpb.SeatMapTask_builder{
		Theater: theater, Auditorium: auditorium, Locale: &locale, TimeZone: &timeZone,
	}.Build())
	return scanner.executor.CaptureSeatMap(ctx, task)
}

func managedScanEgress() *commonpb.EgressPolicy {
	return commonpb.EgressPolicy_builder{ManagedScan: commonpb.ManagedScanEgress_builder{}.Build()}.Build()
}
