package probe

import (
	"context"
	"errors"
	"testing"
	"time"

	central "github.com/cineko-org/contracts/v3"
	"github.com/cineko-org/probe/v2/internal/egress"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	cgvbrowser "github.com/cineko-org/probe/v2/internal/provider/cgv/browser"
)

func TestCGVExecutorCaptureMapping(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC)
	browser := &fakeScheduleBrowser{captures: []cgv.ScheduleCapture{
		{
			TargetDate: "2026-08-12", Complete: true,
			Showtimes: []cgv.ScheduleShowtime{canonicalTestShowtime(cgv.ScheduleShowtime{
				MovieTitle:     "명탐정 코난-하이웨이의 타천사",
				PosterURL:      "https://example.invalid/poster.jpg",
				AuditoriumName: "6관 (Laser)",
				ScreenTypes:    []string{"LASER", "2D"}, Date: "2026-08-12",
				StartsAt: "19:45", EndsAt: "21:44", SoldOut: true, ObservedAt: now.Add(time.Second),
			})},
		},
		{TargetDate: "2026-08-21", Error: cgv.ErrUIContractChanged.Error()},
	}}
	var openedTask cgvbrowser.Task
	openCalls := 0
	executor := &CGVExecutor{
		open: func(_ context.Context, task cgvbrowser.Task) (scheduleBrowser, error) {
			openCalls++
			openedTask = task
			return browser, nil
		},
		clock: func() time.Time { return now },
	}
	task := testAssignmentTask()
	task.TargetDates = []string{"2026-08-12"}
	values, err := executor.Capture(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || !values[0].Complete || values[1].ErrorCode != "ui_contract_changed" ||
		openCalls != 1 || !browser.closed {
		t.Fatalf("captures = %+v, browser opens = %d, closed = %t", values, openCalls, browser.closed)
	}
	if openedTask.Purpose != egress.PurposeScan || openedTask.EgressPolicyID != "scan_default" ||
		!openedTask.Headless || openedTask.Locale != "ko-KR" || openedTask.TimeZone != "Asia/Seoul" {
		t.Fatalf("browser task = %+v", openedTask)
	}
	showtime := values[0].Showtimes[0]
	if showtime.ID != central.CatalogID(central.ProviderCGV, "showtime", showtime.SourceKey) ||
		showtime.ProviderID != central.ProviderCGV || showtime.TheaterID != testAssignmentTask().Theater.ID ||
		showtime.Auditorium.ID != central.CatalogID(central.ProviderCGV, "auditorium", showtime.Auditorium.SourceKey) ||
		showtime.Auditorium.TheaterID != showtime.TheaterID || showtime.Auditorium.Name != "6관 (Laser)" ||
		showtime.Movie.ID != central.CatalogID(central.ProviderCGV, "movie", showtime.Movie.SourceKey) ||
		showtime.Movie.Title != "명탐정 코난-하이웨이의 타천사" || showtime.Movie.PosterURL == "" ||
		showtime.EndsAt.Sub(showtime.StartsAt) != time.Hour+59*time.Minute || !showtime.SoldOut ||
		!values[0].ObservedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("showtime = %+v, observed = %v", showtime, values[0].ObservedAt)
	}
}

func TestCGVExecutorCatalogCapture(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 5, 0, 0, 0, time.UTC)
	browser := &fakeScheduleBrowser{catalog: cgv.CatalogCapture{
		Theaters: []cgv.CatalogTheater{{
			SourceKey: "0056", Region: "서울", Name: "용산아이파크몰",
		}},
		Movies: []cgv.CatalogMovie{{
			SourceKey: "00001234", Title: "어쩔수가없다", PosterURL: "https://example.invalid/poster.jpg",
		}},
	}}
	executor := &CGVExecutor{
		open:  func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) { return browser, nil },
		clock: func() time.Time { return now },
	}
	task := testAssignmentTask()
	task.Kind = central.CapabilityCGVCatalogCapture
	snapshot, err := executor.CaptureCatalog(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if !browser.closed || !snapshot.ObservedAt.Equal(now) || len(snapshot.Theaters) != 1 || len(snapshot.Movies) != 1 {
		t.Fatalf("catalog = %+v, closed = %t", snapshot, browser.closed)
	}
	if snapshot.Theaters[0].ID != central.CatalogID(central.ProviderCGV, "theater", "0056") ||
		snapshot.Movies[0].ID != central.CatalogID(central.ProviderCGV, "movie", "00001234") {
		t.Fatalf("catalog identities = %+v %+v", snapshot.Theaters[0], snapshot.Movies[0])
	}
}

func TestCGVExecutorFailuresAndHelpers(t *testing.T) {
	t.Parallel()
	if _, err := NewCGVExecutor(nil); err == nil {
		t.Fatal("nil browser factory accepted")
	}
	manager, err := egress.New(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := cgvbrowser.New(cgv.DefaultBrowserConfig(), manager)
	if err != nil {
		t.Fatal(err)
	}
	defer factory.Close()
	factoryExecutor, err := NewCGVExecutor(factory)
	if err != nil {
		t.Fatalf("valid browser factory rejected: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factoryExecutor.Capture(cancelled, testAssignmentTask()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled factory capture = %v", err)
	}
	executor := &CGVExecutor{
		open: func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) { return nil, errIO }, clock: time.Now,
	}
	task := testAssignmentTask()
	task.Kind = "unsupported"
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("unsupported task accepted")
	}
	if _, err := executor.CaptureCatalog(context.Background(), task); err == nil {
		t.Fatal("unsupported catalog task accepted")
	}
	task = testAssignmentTask()
	task.EgressPolicyID = ""
	if _, err := executor.Capture(context.Background(), task); !errors.Is(err, central.ErrUnsupportedEgressPolicy) {
		t.Fatalf("missing schedule egress policy error = %v", err)
	}
	task.Kind = central.CapabilityCGVCatalogCapture
	if _, err := executor.CaptureCatalog(context.Background(), task); !errors.Is(err, central.ErrUnsupportedEgressPolicy) {
		t.Fatalf("missing catalog egress policy error = %v", err)
	}
	task = testAssignmentTask()
	task.Theater.ProviderID = "other"
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("non-CGV theater accepted")
	}
	task = testAssignmentTask()
	task.Theater.ID = "theater_not_canonical"
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("non-canonical theater ID accepted")
	}
	task = testAssignmentTask()
	task.TimeZone = "invalid/time-zone"
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("invalid time zone accepted")
	}
	task = testAssignmentTask()
	if _, err := executor.Capture(context.Background(), task); !errors.Is(err, errIO) {
		t.Fatalf("browser open error = %v", err)
	}
	task.Kind = central.CapabilityCGVCatalogCapture
	if _, err := executor.CaptureCatalog(context.Background(), task); !errors.Is(err, errIO) {
		t.Fatalf("catalog browser open error = %v", err)
	}
	executor.open = func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
		return &scheduleOnlyBrowser{}, nil
	}
	if _, err := executor.CaptureCatalog(context.Background(), task); err == nil {
		t.Fatal("catalog-unsupported browser accepted")
	}
	catalogBrowser := &fakeScheduleBrowser{err: errIO}
	executor.open = func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
		return catalogBrowser, nil
	}
	if _, err := executor.CaptureCatalog(context.Background(), task); !errors.Is(err, errIO) || !catalogBrowser.closed {
		t.Fatalf("catalog capture error = %v, closed = %t", err, catalogBrowser.closed)
	}
	task = testAssignmentTask()
	browser := &fakeScheduleBrowser{err: errIO}
	executor.open = func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) { return browser, nil }
	if _, err := executor.Capture(context.Background(), task); !errors.Is(err, errIO) || !browser.closed {
		t.Fatalf("capture error = %v, closed = %t", err, browser.closed)
	}
	browser = &fakeScheduleBrowser{captures: []cgv.ScheduleCapture{{
		TargetDate: "2026-08-20", Complete: true,
		Showtimes: []cgv.ScheduleShowtime{{
			ID: "bad", Date: "bad", StartsAt: "10:00", EndsAt: "11:00",
		}},
	}}}
	executor.open = func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) { return browser, nil }
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("invalid showtime accepted")
	}
	browser.captures[0].Showtimes[0] = canonicalTestShowtime(cgv.ScheduleShowtime{
		Date: "2026-08-20", StartsAt: "10:00", EndsAt: "11:00",
	})
	browser.captures[0].Showtimes[0].Date = "bad"
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("invalid canonical showtime date accepted")
	}
	browser.captures[0].Showtimes[0].Date = "2026-08-20"
	browser.captures[0].Showtimes[0].AuditoriumID = ""
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("empty canonical auditorium ID accepted")
	}
	location := time.FixedZone("KST", 9*60*60)
	if _, _, err := cgv.ParseShowtimeRange("2026-08-20", "bad", "11:00", location); err == nil {
		t.Fatal("invalid start clock accepted")
	}
	if _, _, err := cgv.ParseShowtimeRange("2026-08-20", "10:00", "bad", location); err == nil {
		t.Fatal("invalid end clock accepted")
	}
	startsAt, endsAt, err := cgv.ParseShowtimeRange("2026-08-20", "23:50", "00:20", location)
	if err != nil || endsAt.Sub(startsAt) != 30*time.Minute || endsAt.Day() != 21 {
		t.Fatalf("overnight showtime range = %v - %v, %v", startsAt, endsAt, err)
	}
	if captureErrorCode("") != "capture_incomplete" ||
		captureErrorCode(context.DeadlineExceeded.Error()) != "capture_timeout" ||
		captureErrorCode("other") != "capture_failed" {
		t.Fatal("capture error classification mismatch")
	}
}

func TestCGVExecutorDelegatesSeatMapCapture(t *testing.T) {
	t.Parallel()
	seatMap := &central.SeatMapVersion{ID: "seat_map"}
	delegate := &fakeSeatMapExecutor{seatMap: seatMap}
	executor := &CGVExecutor{seatMap: delegate}
	task := central.AssignmentTask{
		Kind: central.CapabilityCGVSeatMapCapture, EgressPolicyID: central.EgressPolicyScanDefault,
	}
	value, err := executor.CaptureSeatMap(context.Background(), task)
	if err != nil || value != seatMap || delegate.calls != 1 {
		t.Fatalf("seat-map delegation = %+v, %v, calls = %d", value, err, delegate.calls)
	}
	if _, err := (&CGVExecutor{}).CaptureSeatMap(context.Background(), task); err == nil {
		t.Fatal("missing seat-map executor accepted")
	}
	task.EgressPolicyID = ""
	if _, err := executor.CaptureSeatMap(context.Background(), task); !errors.Is(err, central.ErrUnsupportedEgressPolicy) {
		t.Fatalf("missing seat-map egress policy error = %v", err)
	}
	task.EgressPolicyID = central.EgressPolicyScanDefault
	task.Kind = central.CapabilityCGVScheduleCapture
	if _, err := executor.CaptureSeatMap(context.Background(), task); err == nil {
		t.Fatal("wrong seat-map task kind accepted")
	}
}

func TestCGVExecutorCapturesSeatMapWithStandaloneBrowser(t *testing.T) {
	t.Parallel()
	want := &central.SeatMapVersion{ID: "seat_map"}
	browser := &fakeScheduleBrowser{seatMap: want}
	executor := &CGVExecutor{open: func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
		return browser, nil
	}}
	value, err := executor.CaptureSeatMap(context.Background(), central.AssignmentTask{
		Kind: central.CapabilityCGVSeatMapCapture, EgressPolicyID: central.EgressPolicyScanDefault,
	})
	if err != nil || value != want || browser.seatMapCalls != 1 || !browser.closed {
		t.Fatalf("standalone seat-map capture = %+v, error %v, calls %d, closed %v", value, err, browser.seatMapCalls, browser.closed)
	}
	openFailure := &CGVExecutor{open: func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
		return nil, errIO
	}}
	if _, err := openFailure.CaptureSeatMap(context.Background(), central.AssignmentTask{
		Kind: central.CapabilityCGVSeatMapCapture, EgressPolicyID: central.EgressPolicyScanDefault,
	}); !errors.Is(err, errIO) {
		t.Fatalf("standalone browser open error = %v", err)
	}
	unsupported := &CGVExecutor{open: func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
		return &scheduleOnlyBrowser{}, nil
	}}
	if _, err := unsupported.CaptureSeatMap(context.Background(), central.AssignmentTask{
		Kind: central.CapabilityCGVSeatMapCapture, EgressPolicyID: central.EgressPolicyScanDefault,
	}); err == nil {
		t.Fatal("schedule-only browser accepted for seat-map capture")
	}
}

type fakeSeatMapExecutor struct {
	seatMap *central.SeatMapVersion
	calls   int
}

func (executor *fakeSeatMapExecutor) CaptureSeatMap(
	context.Context,
	central.AssignmentTask,
) (*central.SeatMapVersion, error) {
	executor.calls++
	return executor.seatMap, nil
}

func testAssignmentTask() central.AssignmentTask {
	sourceKey := "0056"
	return central.AssignmentTask{
		Kind: central.CapabilityCGVScheduleCapture,
		Theater: central.Theater{
			ID:         central.CatalogID(central.ProviderCGV, "theater", sourceKey),
			ProviderID: central.ProviderCGV, SourceKey: sourceKey, Region: "서울", Name: "용산아이파크몰",
		},
		TargetDates: []string{"2026-08-20"}, Locale: "ko-KR", TimeZone: "Asia/Seoul",
		EgressPolicyID: "scan_default",
	}
}

func canonicalTestShowtime(value cgv.ScheduleShowtime) cgv.ScheduleShowtime {
	theater := testAssignmentTask().Theater
	value.ProviderID = central.ProviderCGV
	value.TheaterID = theater.ID
	value.MovieSourceKey = "00001234"
	value.MovieID = central.CatalogID(central.ProviderCGV, "movie", value.MovieSourceKey)
	value.AuditoriumSourceKey = theater.SourceKey + "/0007"
	value.AuditoriumID = central.CatalogID(central.ProviderCGV, "auditorium", value.AuditoriumSourceKey)
	value.SourceKey = theater.SourceKey + "/2026-08-12/0007/0003"
	value.ID = central.CatalogID(central.ProviderCGV, "showtime", value.SourceKey)
	return value
}

type fakeScheduleBrowser struct {
	captures     []cgv.ScheduleCapture
	catalog      cgv.CatalogCapture
	seatMap      *central.SeatMapVersion
	err          error
	closed       bool
	seatMapCalls int
}

func (browser *fakeScheduleBrowser) CaptureSchedules(
	context.Context,
	cgv.ScheduleTheater,
	[]string,
) ([]cgv.ScheduleCapture, error) {
	return browser.captures, browser.err
}

func (browser *fakeScheduleBrowser) Close() { browser.closed = true }

func (browser *fakeScheduleBrowser) CaptureCatalog(context.Context) (cgv.CatalogCapture, error) {
	return browser.catalog, browser.err
}

func (browser *fakeScheduleBrowser) CaptureSeatMap(
	context.Context,
	central.AssignmentTask,
) (*central.SeatMapVersion, error) {
	browser.seatMapCalls++
	return browser.seatMap, browser.err
}

type scheduleOnlyBrowser struct{}

func (*scheduleOnlyBrowser) CaptureSchedules(
	context.Context,
	cgv.ScheduleTheater,
	[]string,
) ([]cgv.ScheduleCapture, error) {
	return nil, nil
}

func (*scheduleOnlyBrowser) Close() {}

var errIO = errors.New("I/O failure")
