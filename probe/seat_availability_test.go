package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
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

func TestCGVExecutorCapturesSeatAvailability(t *testing.T) {
	t.Parallel()
	task := seatAvailabilityAssignmentTaskForProbe()
	want := validAvailabilitySnapshot()
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

	if _, err := (&CGVExecutor{}).CaptureSeatAvailability(ctx, &observationpb.AssignmentTask{}); err == nil {
		t.Fatal("schedule task accepted as seat-availability capture")
	}

	missingEgress := seatAvailabilityAssignmentTaskForProbe()
	missingEgress.GetEgress().ClearManagedScan()
	if _, err := (&CGVExecutor{}).CaptureSeatAvailability(ctx, missingEgress); err == nil {
		t.Fatal("seat-availability capture without managed scan accepted")
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
	if _, err := unsupported.CaptureSeatAvailability(ctx, seatAvailabilityAssignmentTaskForProbe()); err == nil {
		t.Fatal("schedule-only browser accepted for seat-availability capture")
	}
}

func TestCGVExecutorOpensStandaloneSeatAvailabilityBrowser(t *testing.T) {
	t.Parallel()
	want := validAvailabilitySnapshot()
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
	runtime := newRuntimeForTest(t, api, &fakeProbeSeatAvailabilityExecutor{value: validAvailabilitySnapshot()})
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	if api.committed.GetCompleted() == nil || api.committed.GetCompleted().GetSeatAvailability() == nil {
		t.Fatalf("availability result = %+v", api.committed)
	}

	for _, test := range []struct {
		err       error
		code      string
		retryable bool
	}{
		{err: cgv.ErrProviderAccessBlocked, code: "provider_access_blocked", retryable: true},
		{err: cgv.ErrProviderThrottled, code: "provider_throttled", retryable: true},
		{err: cgv.ErrCaptchaRequired, code: "captcha_required"},
		{err: cgv.ErrAuthenticationRequired, code: "authentication_required"},
		{err: cgv.ErrUIContractChanged, code: "ui_contract_changed"},
		{err: cgv.ErrSeatAvailabilityIncomplete, code: "seat_availability_incomplete"},
		{err: cgv.ErrTargetDateUnavailable, code: "target_date_unavailable"},
		{err: context.DeadlineExceeded, code: "capture_timeout", retryable: true},
	} {
		code, retryable := captureFailure(test.err)
		if code != test.code || retryable != test.retryable {
			t.Fatalf("captureFailure(%v) = %q/%t, want %q/%t", test.err, code, retryable, test.code, test.retryable)
		}
	}
	result, err := runtime.assignmentResult(executionOutput{seatAvailability: &seatmappb.AvailabilitySnapshot{}}, nil, now, now.Add(time.Second))
	if err != nil || result.GetFailed() == nil || result.GetFailed().GetReasonCode() != "invalid_result" {
		t.Fatalf("invalid availability result = %+v, %v", result, err)
	}
}

func TestValidateSeatAvailabilityCaptureRejectsInvalidResults(t *testing.T) {
	t.Parallel()
	task := seatAvailabilityAssignmentTaskForProbe()
	invalid := func(name string, task *observationpb.AssignmentTask, snapshot *seatmappb.AvailabilitySnapshot) {
		t.Helper()
		if err := validateSeatAvailabilityCapture(task, snapshot); err == nil || !errors.Is(err, errInvalidExecutionOutput) {
			t.Fatalf("%s error = %v, want invalid execution output", name, err)
		}
	}

	invalid("missing snapshot", task, nil)
	invalid("invalid snapshot", task, &seatmappb.AvailabilitySnapshot{})
	invalid("missing assignment target", nil, validAvailabilitySnapshot())
	missingShowtime := seatAvailabilityAssignmentTaskForProbe()
	missingShowtime.GetSeatAvailability().ClearShowtime()
	invalid("missing showtime", missingShowtime, validAvailabilitySnapshot())

	mismatched := validAvailabilitySnapshot()
	mismatched.SetShowtimeId("cgv:showtime:other")
	invalid("mismatched identity", task, mismatched)

	invalid("duplicate seats", task, availabilityWithSeats("A", "A"))
	invalid("unsorted seats", task, availabilityWithSeats("B", "A"))
}

type fakeProbeSeatAvailabilityExecutor struct {
	value *seatmappb.AvailabilitySnapshot
	err   error
	calls int
}

func (executor *fakeProbeSeatAvailabilityExecutor) CaptureSeatAvailability(
	context.Context,
	*observationpb.AssignmentTask,
) (*seatmappb.AvailabilitySnapshot, error) {
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
	value  *seatmappb.AvailabilitySnapshot
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
) (*seatmappb.AvailabilitySnapshot, error) {
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
	theater.SetSourceKey(theaterSource)
	theater.SetRegion("서울")
	theater.SetName("용산아이파크몰")
	auditorium := &catalogpb.Auditorium{}
	auditorium.SetId(auditoriumID)
	auditorium.SetTheaterId(theaterID)
	auditorium.SetSourceKey(auditoriumSource)
	auditorium.SetName("IMAX관")
	movie := &catalogpb.Movie{}
	movieSource := "00001234"
	movie.SetId(cgv.CatalogID(cgv.ProviderCGV, "movie", movieSource))
	movie.SetProviderId(cgv.ProviderCGV)
	movie.SetSourceKey(movieSource)
	movie.SetTitle("Example Movie")
	showtimeSource := theaterSource + "/2026-08-21/0007/0003"
	showtime := &catalogpb.Showtime{}
	showtime.SetId(cgv.CatalogID(cgv.ProviderCGV, "showtime", showtimeSource))
	showtime.SetProviderId(cgv.ProviderCGV)
	showtime.SetSourceKey(showtimeSource)
	showtime.SetTheaterId(theaterID)
	showtime.SetMovie(movie)
	showtime.SetAuditorium(auditorium)
	scheduleDate := &commonpb.LocalDate{}
	scheduleDate.SetYear(2026)
	scheduleDate.SetMonth(8)
	scheduleDate.SetDay(21)
	showtime.SetScheduleDate(scheduleDate)
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

func validAvailabilitySnapshot() *seatmappb.AvailabilitySnapshot {
	snapshot := &seatmappb.AvailabilitySnapshot{}
	snapshot.SetShowtimeId(cgv.CatalogID(cgv.ProviderCGV, "showtime", "0056/2026-08-21/0007/0003"))
	snapshot.SetAuditoriumId(cgv.CatalogID(cgv.ProviderCGV, "auditorium", "0056/0007"))
	snapshot.SetLayoutHash(strings.Repeat("a", 64))
	snapshot.SetAvailableSeats([]*seatmappb.AvailableSeat{})
	snapshot.SetObservedAt(timestamppb.Now())
	return snapshot
}

func availabilityWithSeats(ids ...string) *seatmappb.AvailabilitySnapshot {
	snapshot := validAvailabilitySnapshot()
	seats := make([]*seatmappb.AvailableSeat, 0, len(ids))
	for _, id := range ids {
		seat := &seatmappb.AvailableSeat{}
		seat.SetSeatId(id)
		seats = append(seats, seat)
	}
	snapshot.SetAvailableSeats(seats)
	return snapshot
}
