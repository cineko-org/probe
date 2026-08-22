package probe

import (
	"context"
	"errors"
	"testing"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	collectionpb "github.com/cineko-org/contracts/v3/gen/go/cineko/collection"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	seatmappb "github.com/cineko-org/contracts/v3/gen/go/cineko/seatmap"
	"github.com/cineko-org/probe/v2/internal/egress"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	cgvbrowser "github.com/cineko-org/probe/v2/internal/provider/cgv/browser"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCGVExecutorCapturesSeatAvailability(t *testing.T) {
	t.Parallel()
	task := seatAvailabilityAssignmentTaskForProbe()
	want := validLiveSeatObservation(cgv.CatalogID(cgv.ProviderCGV, "showtime", "0056/2026-08-21/0007/0003"), cgv.CatalogID(cgv.ProviderCGV, "auditorium", "0056/0007"))
	delegate := &fakeProbeSeatAvailabilityExecutor{value: want}
	executor := &CGVExecutor{seatAvailability: delegate}
	got, err := executor.CaptureSeatAvailability(context.Background(), task)
	if err != nil || got != want || delegate.calls != 1 {
		t.Fatalf("delegated availability = %+v, %v, calls = %d", got, err, delegate.calls)
	}

	task.GetSeatAvailability().ClearShowtime()
	if _, err := executor.CaptureSeatAvailability(context.Background(), task); err == nil {
		t.Fatal("availability without exact showtime accepted")
	}
}

func TestCGVExecutorSeatAvailabilityValidationAndBrowserFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if _, err := (&CGVExecutor{}).CaptureSeatAvailability(ctx, &observationpb.AssignmentTask{}); !errors.Is(err, errLocalExecution) {
		t.Fatalf("wrong task kind error = %v", err)
	}

	missingEgress := seatAvailabilityAssignmentTaskForProbe()
	missingEgress.GetEgress().ClearManagedScan()
	if _, err := (&CGVExecutor{}).CaptureSeatAvailability(ctx, missingEgress); !errors.Is(err, errLocalExecution) {
		t.Fatalf("missing managed scan error = %v", err)
	}

	if _, err := (&CGVExecutor{}).CaptureSeatAvailability(ctx, seatAvailabilityAssignmentTaskForProbe()); err == nil {
		t.Fatal("seat-availability capture without browser factory accepted")
	}

	openFailure := &CGVExecutor{open: func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
		return nil, errIO
	}}
	if _, err := openFailure.CaptureSeatAvailability(ctx, seatAvailabilityAssignmentTaskForProbe()); !errors.Is(err, errIO) {
		t.Fatalf("seat-availability browser open error = %v", err)
	}

	unsupported := &CGVExecutor{open: func(context.Context, cgvbrowser.Task) (scheduleBrowser, error) {
		return &scheduleOnlyBrowser{}, nil
	}}
	if _, err := unsupported.CaptureSeatAvailability(ctx, seatAvailabilityAssignmentTaskForProbe()); !errors.Is(err, errLocalExecution) {
		t.Fatalf("unsupported browser error = %v", err)
	}
}

func TestCGVExecutorOpensStandaloneSeatAvailabilityBrowser(t *testing.T) {
	t.Parallel()
	want := validLiveSeatObservation(cgv.CatalogID(cgv.ProviderCGV, "showtime", "0056/2026-08-21/0007/0003"), cgv.CatalogID(cgv.ProviderCGV, "auditorium", "0056/0007"))
	browser := &fakeProbeSeatAvailabilityBrowser{value: want}
	var openedTask cgvbrowser.Task
	executor := &CGVExecutor{open: func(_ context.Context, task cgvbrowser.Task) (scheduleBrowser, error) {
		openedTask = task
		return browser, nil
	}}
	got, err := executor.CaptureSeatAvailability(context.Background(), seatAvailabilityAssignmentTaskForProbe())
	if err != nil || got != want || browser.calls != 1 || !browser.closed {
		t.Fatalf("standalone availability = %+v, %v, calls = %d, closed = %t", got, err, browser.calls, browser.closed)
	}
	if openedTask.Purpose != egress.PurposeScan || openedTask.EgressPolicyID != egress.PolicyScanDefault ||
		!openedTask.Headless || openedTask.Locale != "ko-KR" || openedTask.TimeZone != "Asia/Seoul" {
		t.Fatalf("browser task = %+v", openedTask)
	}
}

func TestRuntimeProcessesSeatAvailabilityAndPreservesProtectionReasons(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	assignment := testAssignmentLease("availability_assignment", "lease", now.Add(time.Minute), now.Add(2*time.Minute), seatAvailabilityAssignmentTaskForProbe())
	api := &fakeAPI{}
	assignmentTask := seatAvailabilityAssignmentTaskForProbe()
	runtime := newRuntimeForTest(t, api, &fakeProbeSeatAvailabilityExecutor{value: validLiveSeatObservation(
		assignmentTask.GetSeatAvailability().GetShowtime().GetId(),
		assignmentTask.GetSeatAvailability().GetAuditorium().GetId(),
	)})
	assignment.SetTask(assignmentTask)
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	if api.committed.GetCompleted() == nil || api.committed.GetCompleted().GetLiveSeat() == nil {
		t.Fatalf("availability result = %+v", api.committed)
	}

	for _, test := range []struct {
		err  error
		want func(*collectionpb.FailureReason) bool
	}{
		{err: cgv.ErrIdentityMismatch, want: func(reason *collectionpb.FailureReason) bool { return reason.GetIdentityMismatch() != nil }},
		{err: cgv.ErrProviderAccessBlocked, want: func(reason *collectionpb.FailureReason) bool { return reason.GetProviderBlocked() != nil }},
		{err: cgv.ErrProviderThrottled, want: func(reason *collectionpb.FailureReason) bool { return reason.GetProviderThrottled() != nil }},
		{err: cgv.ErrCaptchaRequired, want: func(reason *collectionpb.FailureReason) bool { return reason.GetCaptchaRequired() != nil }},
		{err: cgv.ErrAuthenticationRequired, want: func(reason *collectionpb.FailureReason) bool { return reason.GetAuthenticationRequired() != nil }},
		{err: cgv.ErrUIContractChanged, want: func(reason *collectionpb.FailureReason) bool { return reason.GetUiContractChanged() != nil }},
		{err: cgv.ErrSeatAvailabilityIncomplete, want: func(reason *collectionpb.FailureReason) bool { return reason.GetInvalidResult() != nil }},
		{err: cgv.ErrProviderInvalidResult, want: func(reason *collectionpb.FailureReason) bool { return reason.GetInvalidResult() != nil }},
		{err: cgv.ErrProviderServerError, want: func(reason *collectionpb.FailureReason) bool { return reason.GetProviderServerError() != nil }},
		{err: context.DeadlineExceeded, want: func(reason *collectionpb.FailureReason) bool { return reason.GetTimeout() != nil }},
		{err: cgv.ErrProviderTransport, want: func(reason *collectionpb.FailureReason) bool { return reason.GetProviderTransportFailed() != nil }},
		{err: errLocalExecution, want: func(reason *collectionpb.FailureReason) bool { return reason.GetInvalidResult() != nil }},
		{err: errors.New("unclassified local failure"), want: func(reason *collectionpb.FailureReason) bool { return reason.GetInvalidResult() != nil }},
	} {
		if !test.want(captureFailureReason(test.err)) {
			t.Fatalf("captureFailureReason(%v) returned the wrong typed reason", test.err)
		}
	}
	deferred := captureDeferredReason(cgv.ErrTargetDateUnavailable)
	if deferred == nil || deferred.GetTargetDateUnavailable() == nil {
		t.Fatal("target-date stopping point was not deferred")
	}
	result, err := runtime.assignmentResult(executionOutput{liveSeat: &seatmappb.LiveSeatObservation{}}, nil, now, now.Add(time.Second))
	if err != nil || result.GetFailed() == nil || result.GetFailed().GetReason().GetInvalidResult() == nil {
		t.Fatalf("invalid availability result = %+v, %v", result, err)
	}
}

func TestValidateSeatAvailabilityCaptureRejectsInvalidResults(t *testing.T) {
	t.Parallel()
	task := seatAvailabilityAssignmentTaskForProbe()
	invalid := func(name string, task *observationpb.AssignmentTask, snapshot *seatmappb.LiveSeatObservation) {
		t.Helper()
		if err := validateLiveSeatCapture(task, snapshot); err == nil || !errors.Is(err, errInvalidExecutionOutput) {
			t.Fatalf("%s error = %v, want invalid execution output", name, err)
		}
	}

	invalid("missing snapshot", task, nil)
	invalid("invalid snapshot", task, &seatmappb.LiveSeatObservation{})
	invalid("missing assignment target", nil, validLiveSeatObservation("showtime", "auditorium"))
	missingShowtime := seatAvailabilityAssignmentTaskForProbe()
	missingShowtime.GetSeatAvailability().ClearShowtime()
	invalid("missing showtime", missingShowtime, validLiveSeatObservation("showtime", "auditorium"))

	mismatched := validLiveSeatObservation("showtime", "auditorium")
	mismatched.GetAvailability().SetShowtimeId("other")
	invalid("mismatched identity", task, mismatched)
}

type fakeProbeSeatAvailabilityExecutor struct {
	value *seatmappb.LiveSeatObservation
	err   error
	calls int
}

func (executor *fakeProbeSeatAvailabilityExecutor) CaptureSeatAvailability(
	context.Context,
	*observationpb.AssignmentTask,
) (*seatmappb.LiveSeatObservation, error) {
	executor.calls++
	return executor.value, executor.err
}

func (executor *fakeProbeSeatAvailabilityExecutor) Capture(
	context.Context,
	*observationpb.AssignmentTask,
) ([]*observationpb.Capture, error) {
	return nil, errors.New("schedule capture is not part of this test executor")
}

type fakeProbeSeatAvailabilityBrowser struct {
	value  *seatmappb.LiveSeatObservation
	err    error
	calls  int
	closed bool
}

func (browser *fakeProbeSeatAvailabilityBrowser) CaptureSchedules(
	context.Context,
	cgv.ScheduleTheater,
	[]string,
) ([]cgv.ScheduleCapture, error) {
	return nil, nil
}

func (browser *fakeProbeSeatAvailabilityBrowser) Close() { browser.closed = true }

func (browser *fakeProbeSeatAvailabilityBrowser) CaptureSeatAvailability(
	context.Context,
	*observationpb.AssignmentTask,
) (*seatmappb.LiveSeatObservation, error) {
	browser.calls++
	return browser.value, browser.err
}

func seatAvailabilityAssignmentTaskForProbe() *observationpb.AssignmentTask {
	theaterSource := "0056"
	theaterID := cgv.CatalogID(cgv.ProviderCGV, "theater", theaterSource)
	auditoriumSource := theaterSource + "/0007"
	auditoriumID := cgv.CatalogID(cgv.ProviderCGV, "auditorium", auditoriumSource)
	theater := &catalogpb.Theater{}
	theater.SetId(theaterID)
	theater.SetProviderId(cgv.ProviderCGV)
	theater.SetIdentity(cgv.NewTheaterIdentity(theaterSource))
	theater.SetRegion("서울")
	theater.SetName("용산아이파크몰")
	auditorium := &catalogpb.Auditorium{}
	auditorium.SetId(auditoriumID)
	auditorium.SetTheaterId(theaterID)
	auditorium.SetIdentity(cgv.NewAuditoriumIdentity(theaterSource, "0007"))
	auditorium.SetName("IMAX관")
	movie := &catalogpb.Movie{}
	movieSource := "00001234"
	movie.SetId(cgv.CatalogID(cgv.ProviderCGV, "movie", movieSource))
	movie.SetProviderId(cgv.ProviderCGV)
	movie.SetIdentity(cgv.NewMovieIdentity(movieSource))
	movie.SetTitle("Example Movie")
	showtimeSource := theaterSource + "/2026-08-21/0007/0003"
	showtime := &catalogpb.Showtime{}
	showtime.SetId(cgv.CatalogID(cgv.ProviderCGV, "showtime", showtimeSource))
	showtime.SetProviderId(cgv.ProviderCGV)
	showtime.SetTheaterId(theaterID)
	showtime.SetMovie(movie)
	showtime.SetAuditorium(auditorium)
	showtimeIdentity, err := cgv.NewShowtimeIdentity(theaterSource, "2026-08-21", "0007", "0003")
	if err != nil {
		panic(err)
	}
	showtime.SetIdentity(showtimeIdentity)
	startsAt := time.Date(2026, 8, 21, 20, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	showtime.SetStartsAt(timestamppb.New(startsAt))
	showtime.SetEndsAt(timestamppb.New(startsAt.Add(2 * time.Hour)))
	seatTask := &observationpb.SeatAvailabilityTask{}
	seatTask.SetTheater(theater)
	seatTask.SetAuditorium(auditorium)
	seatTask.SetShowtime(showtime)
	seatTask.SetLocale("ko-KR")
	seatTask.SetTimeZone("Asia/Seoul")
	assignment := &observationpb.AssignmentTask{}
	assignment.SetSeatAvailability(seatTask)
	egressPolicy := &commonpb.EgressPolicy{}
	egressPolicy.SetManagedScan(&commonpb.ManagedScanEgress{})
	assignment.SetEgress(egressPolicy)
	return assignment
}
