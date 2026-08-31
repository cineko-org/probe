package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
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
	values, err := executor.Capture(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || !values[0].GetComplete() || values[1].GetErrorCode() != "ui_contract_changed" ||
		openCalls != 1 || !browser.closed {
		t.Fatalf("captures = %+v, browser opens = %d, closed = %t", values, openCalls, browser.closed)
	}
	if openedTask.Purpose != egress.PurposeScan || openedTask.EgressPolicyID != "scan_default" ||
		!openedTask.Headless || openedTask.Locale != "ko-KR" || openedTask.TimeZone != "Asia/Seoul" {
		t.Fatalf("browser task = %+v", openedTask)
	}
	showtime := values[0].GetShowtimes()[0]
	showtimeSite, showtimeDate, showtimeScreen, showtimeSequence, showtimeIdentityOK := cgv.ShowtimeIdentityValues(showtime)
	auditoriumSite, auditoriumScreen, auditoriumIdentityOK := cgv.AuditoriumIdentityValues(showtime.GetAuditorium())
	movieNo := showtime.GetMovie().GetIdentity().GetCgv().GetMovieNo()
	if !showtimeIdentityOK || showtime.GetId() != cgv.CatalogID(cgv.ProviderCGV, "showtime", strings.Join([]string{showtimeSite, showtimeDate, showtimeScreen, showtimeSequence}, "/")) ||
		showtime.GetProviderId() != cgv.ProviderCGV || showtime.GetTheaterId() != testAssignmentTask().GetSchedule().GetTheater().GetId() ||
		!auditoriumIdentityOK || showtime.GetAuditorium().GetId() != cgv.CatalogID(cgv.ProviderCGV, "auditorium", auditoriumSite+"/"+auditoriumScreen) ||
		showtime.GetAuditorium().GetTheaterId() != showtime.GetTheaterId() || showtime.GetAuditorium().GetName() != "6관 (Laser)" ||
		showtime.GetMovie().GetId() != cgv.CatalogID(cgv.ProviderCGV, "movie", movieNo) ||
		showtime.GetMovie().GetTitle() != "명탐정 코난-하이웨이의 타천사" || showtime.GetMovie().GetPosterUrl() != "" ||
		showtime.GetEndsAt().AsTime().Sub(showtime.GetStartsAt().AsTime()) != time.Hour+59*time.Minute || !showtime.GetSoldOut() ||
		!values[0].GetObservedAt().AsTime().Equal(now.Add(time.Second)) {
		t.Fatalf("showtime = %+v, observed = %v", showtime, values[0].GetObservedAt())
	}
}

func TestCGVExecutorCaptureWeekdaysUsesFilteredBrowserPath(t *testing.T) {
	t.Parallel()
	browser := &fakeScheduleBrowser{captures: []cgv.ScheduleCapture{{TargetDate: "2026-08-24", Complete: true}}}
	executor := &CGVExecutor{
		open:  func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) { return browser, nil },
		clock: time.Now,
	}
	values, err := executor.CaptureWeekdays(context.Background(), testAssignmentTask(), []int32{1, 3, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || len(browser.weekdays) != 2 || browser.weekdays[0] != time.Monday || browser.weekdays[1] != time.Wednesday {
		t.Fatalf("captures = %+v, weekdays = %v", values, browser.weekdays)
	}
	if _, err := executor.CaptureWeekdays(context.Background(), testAssignmentTask(), []int32{7}); !errors.Is(err, errLocalExecution) {
		t.Fatalf("invalid weekday error = %v", err)
	}
}

func TestCGVExecutorScheduleSessionReusesOneBrowser(t *testing.T) {
	t.Parallel()
	browser := &fakeScheduleBrowser{captures: []cgv.ScheduleCapture{{TargetDate: "2026-08-24", Complete: true}}}
	openCalls := 0
	executor := &CGVExecutor{
		open: func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
			openCalls++
			return browser, nil
		},
		clock: time.Now,
	}
	session, err := executor.OpenScheduleSession(context.Background(), testAssignmentTask())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		captures, captureErr := session.CaptureWeekdays(context.Background(), testAssignmentTask(), []int32{1})
		if captureErr != nil || len(captures) != 1 {
			t.Fatalf("captures = %+v, error = %v", captures, captureErr)
		}
	}
	if openCalls != 1 || browser.closed {
		t.Fatalf("browser opens = %d, closed before session close = %t", openCalls, browser.closed)
	}
	session.Close()
	session.Close()
	if !browser.closed {
		t.Fatal("session close did not close its browser")
	}
	if _, err := session.Capture(context.Background(), testAssignmentTask()); err == nil {
		t.Fatal("closed session accepted a capture")
	}
}

func TestCGVExecutorScheduleSessionUsesOneDateShard(t *testing.T) {
	t.Parallel()
	browser := &fakeScheduleBrowser{captures: []cgv.ScheduleCapture{{TargetDate: "2026-08-29", Complete: true}}}
	executor := &CGVExecutor{
		open:  func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) { return browser, nil },
		clock: time.Now,
	}
	session, err := executor.OpenScheduleSession(context.Background(), testAssignmentTask())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	captures, err := session.CaptureWeekdayShard(context.Background(), testAssignmentTask(), []int32{5, 6}, 7)
	if err != nil || len(captures) != 1 {
		t.Fatalf("captures = %+v, error = %v", captures, err)
	}
	if browser.shard != 7 || len(browser.weekdays) != 2 || browser.weekdays[0] != time.Friday || browser.weekdays[1] != time.Saturday {
		t.Fatalf("shard/weekdays = %d/%v", browser.shard, browser.weekdays)
	}
}

func TestValidateCanonicalShowtimeRejectsRelationalIdentityMismatch(t *testing.T) {
	t.Parallel()
	showtime := canonicalTestShowtime(cgv.ScheduleShowtime{})
	if err := validateCanonicalShowtime(showtime); err != nil {
		t.Fatalf("canonical showtime rejected: %v", err)
	}
	showtime.AuditoriumSourceKey = "0056/0008"
	showtime.AuditoriumID = cgv.CatalogID(cgv.ProviderCGV, "auditorium", showtime.AuditoriumSourceKey)
	if err := validateCanonicalShowtime(showtime); !errors.Is(err, cgv.ErrIdentityMismatch) {
		t.Fatalf("showtime/auditorium relation error = %v", err)
	}
}

func TestCanonicalShowtimeIdentityBoundaryBranches(t *testing.T) {
	t.Parallel()

	showtime := canonicalTestShowtime(cgv.ScheduleShowtime{})
	showtime.TheaterID = "theater_not_canonical"
	if _, _, err := canonicalShowtimeContract(showtime); !errors.Is(err, cgv.ErrIdentityMismatch) {
		t.Fatalf("non-canonical theater identity error = %v", err)
	}

	for _, sourceKey := range []string{
		"0056/2026-08-12/0007",
		"site/2026-08-12/0007/0003",
		"0056/2026-08-12/screen/0003",
		"0056/2026-08-12/0007/sequence",
	} {
		if _, err := canonicalShowtimeSourceParts(sourceKey); !errors.Is(err, cgv.ErrIdentityMismatch) {
			t.Fatalf("non-canonical showtime source %q error = %v", sourceKey, err)
		}
	}
	if numericIdentifier(" ") || numericIdentifier("12x") || !numericIdentifier("0012") {
		t.Fatal("numeric identifier boundary classification mismatch")
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
			SourceKey: "00001234", Title: "어쩔수가없다",
		}},
		Posters: []cgv.CatalogPoster{{
			MovieSourceKey: "00001234", MediaType: "image/jpeg", Data: []byte("poster"), ContentHash: strings.Repeat("a", 64),
		}},
	}}
	executor := &CGVExecutor{
		open:  func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) { return browser, nil },
		clock: func() time.Time { return now },
	}
	task := testAssignmentTask()
	setCatalogTask(task)
	snapshot, err := executor.CaptureCatalog(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if !browser.closed || !snapshot.GetObservedAt().AsTime().Equal(now) || len(snapshot.GetTheaters()) != 1 || len(snapshot.GetMovies()) != 1 || len(snapshot.GetPosters()) != 1 {
		t.Fatalf("catalog = %+v, closed = %t", snapshot, browser.closed)
	}
	if snapshot.GetTheaters()[0].GetId() != cgv.CatalogID(cgv.ProviderCGV, "theater", "0056") ||
		snapshot.GetMovies()[0].GetId() != cgv.CatalogID(cgv.ProviderCGV, "movie", "00001234") {
		t.Fatalf("catalog identities = %+v %+v", snapshot.GetTheaters()[0], snapshot.GetMovies()[0])
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
	task := &observationpb.AssignmentTask{}
	if _, err := executor.Capture(context.Background(), task); !errors.Is(err, errLocalExecution) {
		t.Fatalf("unsupported task error = %v", err)
	}
	if _, err := executor.CaptureCatalog(context.Background(), task); !errors.Is(err, errLocalExecution) {
		t.Fatalf("unsupported catalog task error = %v", err)
	}
	task = testAssignmentTask()
	task.GetEgress().ClearManagedScan()
	if _, err := executor.Capture(context.Background(), task); !errors.Is(err, errLocalExecution) {
		t.Fatalf("missing schedule egress policy error = %v", err)
	}
	setCatalogTask(task)
	if _, err := executor.CaptureCatalog(context.Background(), task); !errors.Is(err, errLocalExecution) {
		t.Fatalf("missing catalog egress policy error = %v", err)
	}
	task = testAssignmentTask()
	setCatalogTask(task)
	task.GetCatalog().SetProviderId("other")
	if _, err := executor.CaptureCatalog(context.Background(), task); !errors.Is(err, errLocalExecution) {
		t.Fatalf("unsupported catalog provider error = %v", err)
	}
	task = testAssignmentTask()
	task.GetSchedule().GetTheater().SetProviderId("other")
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("non-CGV theater accepted")
	}
	task = testAssignmentTask()
	task.GetSchedule().GetTheater().SetId("theater_not_canonical")
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("non-canonical theater ID accepted")
	}
	task = testAssignmentTask()
	task.GetSchedule().SetTimeZone("invalid/time-zone")
	if _, err := executor.Capture(context.Background(), task); err == nil {
		t.Fatal("invalid time zone accepted")
	}
	task = testAssignmentTask()
	if _, err := executor.Capture(context.Background(), task); !errors.Is(err, errIO) {
		t.Fatalf("browser open error = %v", err)
	}
	setCatalogTask(task)
	if _, err := executor.CaptureCatalog(context.Background(), task); !errors.Is(err, errIO) {
		t.Fatalf("catalog browser open error = %v", err)
	}
	executor.open = func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
		return &scheduleOnlyBrowser{}, nil
	}
	if _, err := executor.CaptureCatalog(context.Background(), task); !errors.Is(err, errLocalExecution) {
		t.Fatalf("catalog-unsupported browser error = %v", err)
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

func testAssignmentTask() *observationpb.AssignmentTask {
	sourceKey := "0056"
	theater := &catalogpb.Theater{}
	theater.SetId(cgv.CatalogID(cgv.ProviderCGV, "theater", sourceKey))
	theater.SetProviderId(cgv.ProviderCGV)
	theater.SetIdentity(cgv.NewTheaterIdentity(sourceKey))
	theater.SetRegion("서울")
	theater.SetName("용산아이파크몰")
	schedule := &observationpb.ScheduleTask{}
	schedule.SetTheater(theater)
	schedule.SetLocale("ko-KR")
	schedule.SetTimeZone("Asia/Seoul")
	task := &observationpb.AssignmentTask{}
	egressPolicy := &commonpb.EgressPolicy{}
	egressPolicy.SetManagedScan(&commonpb.ManagedScanEgress{})
	task.SetEgress(egressPolicy)
	task.SetSchedule(schedule)
	return task
}

func sourceKeyForTheater(theater *catalogpb.Theater) string {
	value, _ := cgv.TheaterSiteNo(theater)
	return value
}

func canonicalTestShowtime(value cgv.ScheduleShowtime) cgv.ScheduleShowtime {
	theater := testAssignmentTask().GetSchedule().GetTheater()
	value.ProviderID = cgv.ProviderCGV
	value.TheaterID = theater.GetId()
	value.MovieSourceKey = "00001234"
	value.MovieID = cgv.CatalogID(cgv.ProviderCGV, "movie", value.MovieSourceKey)
	value.AuditoriumSourceKey = sourceKeyForTheater(theater) + "/0007"
	value.AuditoriumID = cgv.CatalogID(cgv.ProviderCGV, "auditorium", value.AuditoriumSourceKey)
	value.SourceKey = sourceKeyForTheater(theater) + "/2026-08-12/0007/0003"
	value.ID = cgv.CatalogID(cgv.ProviderCGV, "showtime", value.SourceKey)
	return value
}

type fakeScheduleBrowser struct {
	captures []cgv.ScheduleCapture
	catalog  cgv.CatalogCapture
	weekdays []time.Weekday
	shard    int
	err      error
	closed   bool
}

func (browser *fakeScheduleBrowser) CaptureScheduleWeekdayShard(
	_ context.Context,
	_ cgv.ScheduleTheater,
	weekdays []time.Weekday,
	shard int,
) ([]cgv.ScheduleCapture, error) {
	browser.weekdays = append([]time.Weekday(nil), weekdays...)
	browser.shard = shard
	return browser.captures, browser.err
}

func (browser *fakeScheduleBrowser) CaptureSchedules(
	context.Context,
	cgv.ScheduleTheater,
	[]string,
) ([]cgv.ScheduleCapture, error) {
	return browser.captures, browser.err
}

func (browser *fakeScheduleBrowser) CaptureSchedulesForWeekdays(
	_ context.Context,
	_ cgv.ScheduleTheater,
	weekdays []time.Weekday,
) ([]cgv.ScheduleCapture, error) {
	browser.weekdays = append([]time.Weekday(nil), weekdays...)
	return browser.captures, browser.err
}

func (browser *fakeScheduleBrowser) Close() { browser.closed = true }

func (browser *fakeScheduleBrowser) CaptureCatalog(context.Context, []string) (cgv.CatalogCapture, error) {
	return browser.catalog, browser.err
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

func setCatalogTask(task *observationpb.AssignmentTask) {
	catalog := &observationpb.CatalogTask{}
	base := testAssignmentTask().GetSchedule()
	catalog.SetProviderId(cgv.ProviderCGV)
	catalog.SetLocale(base.GetLocale())
	catalog.SetTimeZone(base.GetTimeZone())
	task.SetCatalog(catalog)
}
