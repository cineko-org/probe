package probe

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	probepb "github.com/cineko-org/contracts/v3/gen/go/cineko/probe"
	seatmappb "github.com/cineko-org/contracts/v3/gen/go/cineko/seatmap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRuntimeProcessesAssignmentAndDisconnects(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeAPI{}
	api.assignment = claimResponse(testAssignmentLease("assignment_01", "lease_01", time.Now().Add(time.Minute), time.Now().Add(2*time.Minute), testAssignmentTask()))
	api.onCommit = cancel
	executor := &fakeExecutor{captures: []*observationpb.Capture{capture(true)}}
	runtime := newRuntimeForTest(t, api, executor)
	if err := runtime.Run(ctx); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.registerCalls != 1 || api.claimCalls == 0 || api.commitCalls != 1 || api.disconnectCalls != 1 ||
		api.committed.GetCompleted() == nil || api.committed.GetRunId() == "" || executor.calls != 1 {
		t.Fatalf("API = %+v, result = %+v, executor calls = %d", api, api.committed, executor.calls)
	}
}

func TestRuntimeReportsPartialAssignment(t *testing.T) {
	t.Parallel()
	now := time.Now()
	api := &fakeAPI{}
	runtime := newRuntimeForTest(t, api, &fakeExecutor{captures: []*observationpb.Capture{
		capture(true), capture(false),
	}})
	assignment := testAssignmentLease("assignment_partial", "lease", now.Add(time.Minute), now.Add(2*time.Minute), testAssignmentTask())
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	if resultOutcome(api.committed) != "degraded" {
		t.Fatalf("result status = %q", resultOutcome(api.committed))
	}
}

func TestRuntimeProcessesCatalogAssignmentAndRejectsUnsupportedExecutor(t *testing.T) {
	t.Parallel()
	now := time.Now()
	assignment := testAssignmentLease("catalog_assignment", "lease", now.Add(time.Minute), now.Add(2*time.Minute), testAssignmentTask())
	assignment.SetTask(catalogAssignmentTask())
	catalog := catalogSnapshot(now)
	api := &fakeAPI{}
	executor := &fakeExecutor{catalog: catalog}
	runtime := newRuntimeForTest(t, api, executor)
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	committed := api.committed
	api.mu.Unlock()
	if committed.GetCompleted() == nil || committed.GetCompleted().GetCatalog() != catalog || committed.GetCompleted().GetSchedule() != nil ||
		executor.catalogCalls != 1 || executor.calls != 0 {
		t.Fatalf("catalog result = %+v, schedule calls = %d, catalog calls = %d", committed, executor.calls, executor.catalogCalls)
	}

	api = &fakeAPI{}
	runtime = newRuntimeForTest(t, api, contextIgnoringExecutor{wait: make(chan struct{})})
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	committed = api.committed
	api.mu.Unlock()
	if committed.GetFailed() == nil || committed.GetCompleted().GetCatalog() != nil {
		t.Fatalf("unsupported catalog result = %+v", committed)
	}

	api = &fakeAPI{}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{catalogErr: errIO})
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	committed = api.committed
	api.mu.Unlock()
	if committed.GetFailed() == nil || committed.GetCompleted().GetCatalog() != nil {
		t.Fatalf("failed catalog result = %+v", committed)
	}
}

func TestRuntimeProcessesSeatMapAssignment(t *testing.T) {
	t.Parallel()
	now := time.Now()
	assignment := testAssignmentLease("seat_assignment", "lease", now.Add(time.Minute), now.Add(2*time.Minute), testAssignmentTask())
	assignment.SetTask(seatMapAssignmentTask())
	seatMapTask := seatMapAssignmentTask().GetSeatMap()
	seatMap := validLiveSeatObservation(seatMapTask.GetShowtime().GetId(), seatMapTask.GetAuditorium().GetId())
	api := &fakeAPI{}
	executor := &fakeExecutor{liveSeat: seatMap}
	runtime := newRuntimeForTest(t, api, executor)
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	if api.committed.GetCompleted() == nil || api.committed.GetCompleted().GetLiveSeat() != seatMap || executor.seatMapCalls != 1 {
		t.Fatalf("seat-map result = %+v, calls = %d", api.committed, executor.seatMapCalls)
	}

	api = &fakeAPI{}
	runtime = newRuntimeForTest(t, api, contextIgnoringExecutor{wait: make(chan struct{})})
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	if api.committed.GetFailed() == nil || api.committed.GetCompleted().GetLiveSeat() != nil {
		t.Fatalf("unsupported seat-map result = %+v", api.committed)
	}
}

func TestRuntimeReportsReadiness(t *testing.T) {
	t.Parallel()
	runtime := newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	if err := runtime.RunReady(context.Background(), nil); err == nil {
		t.Fatal("nil readiness channel accepted")
	}
	invalidReady := make(chan error, 1)
	if err := runtime.RunReady(nil, invalidReady); err == nil { //nolint:staticcheck // Explicit nil boundary.
		t.Fatal("nil context accepted")
	}
	if err := <-invalidReady; err == nil {
		t.Fatal("nil context readiness did not report failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- runtime.RunReady(ctx, ready) }()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	api, ok := runtime.api.(*fakeAPI)
	if !ok {
		t.Fatalf("runtime API type = %T", runtime.api)
	}
	api.mu.Lock()
	heartbeatCalls := api.heartbeatCalls
	api.mu.Unlock()
	if heartbeatCalls != 1 {
		t.Fatalf("readiness reported before initial heartbeat: calls = %d", heartbeatCalls)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	terminal := &APIError{StatusCode: 400, Code: "invalid"}
	runtime = newRuntimeForTest(t, &fakeAPI{heartbeatErrors: []error{terminal}}, &fakeExecutor{})
	failedReady := make(chan error, 1)
	if err := runtime.RunReady(context.Background(), failedReady); !errors.Is(err, terminal) {
		t.Fatalf("RunReady() error = %v", err)
	}
	if err := <-failedReady; !errors.Is(err, terminal) {
		t.Fatalf("readiness error = %v", err)
	}
}

func TestRuntimeRenewsLeaseWhileCaptureRuns(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{}
	release := make(chan struct{})
	api.onAssignmentHeartbeat = func() { close(release) }
	executor := &fakeExecutor{wait: release, captures: []*observationpb.Capture{capture(true)}}
	runtime := newRuntimeForTest(t, api, executor)
	assignment := testAssignmentLease("assignment", "lease", time.Now().Add(3*time.Second), time.Now().Add(time.Minute), testAssignmentTask())
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.assignmentHeartbeatCalls != 1 || api.commitCalls != 1 {
		t.Fatalf("lease heartbeat calls = %d, commits = %d", api.assignmentHeartbeatCalls, api.commitCalls)
	}
}

func TestRuntimeRejectsAssignmentClaimedDuringDrainTransition(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor := &fakeExecutor{}
	api := &fakeAPI{assignment: claimResponse(testAssignmentLease("assignment_during_drain", "lease", time.Now().Add(time.Minute), time.Now().Add(time.Minute), testAssignmentTask()))}
	runtime := newRuntimeForTest(t, api, executor)
	api.onClaim = func() { runtime.SetDraining(true) }
	api.onCommit = cancel
	if err := runtime.workLoop(ctx, Session{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("workLoop() error = %v", err)
	}
	runtime.random = errorReader{}
	if err := runtime.rejectClaimWhileDraining(context.Background(), Session{}, &probepb.AssignmentLease{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("drained claim run ID error = %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if executor.calls != 0 || api.commitCalls != 1 {
		t.Fatalf("executor calls = %d, commits = %d", executor.calls, api.commitCalls)
	}
	if api.committed.GetFailed() == nil || api.committed.GetRunId() == "" ||
		api.committed.GetStartedAt().AsTime().IsZero() || api.committed.GetFinishedAt().AsTime().IsZero() ||
		api.committed.GetFailed().GetReason().GetInvalidResult() == nil {
		t.Fatalf("drained claim result = %+v", api.committed)
	}
}

func TestRuntimeRegistrationRetryAndReauthentication(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{registerErrors: []error{&APIError{StatusCode: 503, Code: "unavailable", Retryable: true}}}
	runtime := newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.wait = func(context.Context, time.Duration) error { return nil }
	session, interval, err := runtime.register(context.Background())
	if err != nil || session.ProbeID == "" || interval != time.Second {
		t.Fatalf("registration = %+v, %v, %v", session, interval, err)
	}
	if api.registerCalls != 2 {
		t.Fatalf("registration calls = %d", api.registerCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	api = &fakeAPI{heartbeatErrors: []error{ErrUnauthorized}, cancelOnHeartbeat: cancel}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if err := runtime.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if api.registerCalls != 2 || api.disconnectCalls != 2 {
		t.Fatalf("reauth registrations = %d, disconnects = %d", api.registerCalls, api.disconnectCalls)
	}

	ctx, cancel = context.WithCancel(context.Background())
	api = &fakeAPI{claimErrors: []error{ErrUnauthorized}, cancelOnHeartbeat: cancel}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if err := runtime.Run(ctx); err != nil || api.registerCalls != 2 || api.disconnectCalls != 2 {
		t.Fatalf("session reauth = %v, registrations = %d, disconnects = %d", err, api.registerCalls, api.disconnectCalls)
	}

	terminalClaim := &APIError{StatusCode: 400, Code: "invalid"}
	runtime = newRuntimeForTest(t, &fakeAPI{claimErrors: []error{terminalClaim}}, &fakeExecutor{})
	if err := runtime.Run(context.Background()); !errors.Is(err, terminalClaim) {
		t.Fatalf("terminal session error = %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	api = &fakeAPI{heartbeatErrors: []error{io.ErrUnexpectedEOF}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.wait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	if err := runtime.Run(ctx); err != nil || api.disconnectCalls != 1 {
		t.Fatalf("cancelled initial heartbeat = %v, disconnects = %d", err, api.disconnectCalls)
	}
}

func TestRuntimeAssignmentFailureBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Now()
	assignment := testAssignmentLease("assignment", "lease", now.Add(time.Minute), now.Add(time.Minute), testAssignmentTask())
	for _, expired := range []*probepb.AssignmentLease{
		testAssignmentLease("assignment", "lease", now.Add(-time.Second), now.Add(time.Minute), testAssignmentTask()),
		testAssignmentLease("assignment", "lease", now.Add(time.Minute), now.Add(-time.Second), testAssignmentTask()),
	} {
		runtime := newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
		if err := runtime.executeAssignment(context.Background(), Session{}, expired); !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("expired assignment error = %v", err)
		}
	}
	api := &fakeAPI{commitErrors: []error{&APIError{StatusCode: 503, Code: "retry", Retryable: true}}}
	runtime := newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.wait = func(context.Context, time.Duration) error { return nil }
	result, err := runtime.assignmentResult(executionOutput{}, io.ErrUnexpectedEOF, now, now.Add(time.Second))
	if err != nil || result.GetFailed() == nil {
		t.Fatalf("failed result = %+v, %v", result, err)
	}
	if err := runtime.commitResult(context.Background(), Session{}, assignment, result); err != nil {
		t.Fatal(err)
	}
	if api.commitCalls != 2 {
		t.Fatalf("commit attempts = %d", api.commitCalls)
	}
	for _, commitErr := range []error{ErrUnauthorized, ErrLeaseExpired, &APIError{StatusCode: 400, Code: "bad"}} {
		api := &fakeAPI{commitErrors: []error{commitErr}}
		runtime := newRuntimeForTest(t, api, &fakeExecutor{})
		err := runtime.commitResult(context.Background(), Session{}, assignment, result)
		if !errors.Is(err, commitErr) {
			t.Fatalf("commit error %v became %v", commitErr, err)
		}
	}
}

func TestRuntimeStateConfigurationAndHelpers(t *testing.T) {
	t.Parallel()
	valid := Config{Registration: testRegistration()}
	if _, err := NewRuntime(nil, StaticCredential("token"), &fakeExecutor{}, valid); err == nil {
		t.Fatal("nil API accepted")
	}
	invalidConcurrency := Config{Registration: testRegistration()}
	invalidConcurrency.Registration.SetMaxConcurrency(2)
	if _, err := NewRuntime(&fakeAPI{}, StaticCredential("token"), &fakeExecutor{}, invalidConcurrency); err == nil {
		t.Fatal("invalid concurrency accepted")
	}
	invalidKind := Config{Registration: testRegistration()}
	invalidKind.Registration.SetKind(&probepb.ProbeKind{})
	if _, err := NewRuntime(&fakeAPI{}, StaticCredential("token"), &fakeExecutor{}, invalidKind); err == nil {
		t.Fatal("invalid kind accepted")
	}
	invalidIntervals := valid
	invalidIntervals.ClaimMinimum = 2 * time.Second
	invalidIntervals.ClaimMaximum = time.Second
	if _, err := NewRuntime(&fakeAPI{}, StaticCredential("token"), &fakeExecutor{}, invalidIntervals); err == nil {
		t.Fatal("invalid intervals accepted")
	}
	invalidHeartbeatLimit := valid
	invalidHeartbeatLimit.HeartbeatFailureLimit = -1
	if _, err := NewRuntime(&fakeAPI{}, StaticCredential("token"), &fakeExecutor{}, invalidHeartbeatLimit); err == nil {
		t.Fatal("invalid heartbeat failure limit accepted")
	}
	runtime := newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	if err := runtime.Run(nil); err == nil { //nolint:staticcheck // Explicitly verifies the nil-context boundary.
		t.Fatal("nil runtime context accepted")
	}
	runtime.SetDraining(true)
	state := runtime.heartbeatState()
	if !state.GetDraining() || state.GetAvailableSlots() != 0 {
		t.Fatalf("draining heartbeat = %+v", state)
	}
	runtime.SetDraining(false)
	runtime.setActive("assignment")
	state = runtime.heartbeatState()
	if state.GetAvailableSlots() != 0 || len(state.GetActiveAssignmentIds()) != 1 {
		t.Fatalf("active heartbeat = %+v", state)
	}
	runtime.setActive("")
	runtime.remoteDrain = true
	if !runtime.draining() {
		t.Fatal("remote drain was ignored")
	}
	if resultOutcome(resultForOutput(executionOutput{})) != "failed" ||
		resultOutcome(resultForOutput(executionOutput{catalog: &catalogpb.CatalogSnapshot{}})) != "succeeded" ||
		resultOutcome(resultForOutput(executionOutput{liveSeat: validLiveSeatObservation("showtime", "auditorium")})) != "succeeded" ||
		resultOutcome(resultForOutput(executionOutput{captures: []*observationpb.Capture{capture(true), capture(false)}})) != "degraded" ||
		resultOutcome(resultForOutput(executionOutput{captures: []*observationpb.Capture{capture(false)}})) != "failed" ||
		resultOutcome(resultForOutput(executionOutput{captures: []*observationpb.Capture{capture(true)}})) != "succeeded" {
		t.Fatal("result status classification mismatch")
	}
	scheduleCapability := &observationpb.Capability{}
	scheduleCapability.SetScheduleCapture(&observationpb.ScheduleCapture{})
	seatMapCapability := &observationpb.Capability{}
	seatMapCapability.SetSeatMapCapture(&observationpb.SeatMapCapture{})
	emptyCapability := &observationpb.Capability{}
	runtime.config.Registration.SetCapabilities([]*observationpb.Capability{scheduleCapability, seatMapCapability})
	runtime.config.AvailableCapabilities = func() []*observationpb.Capability {
		return []*observationpb.Capability{seatMapCapability, emptyCapability, seatMapCapability}
	}
	if available := runtime.availableCapabilities(); len(available) != 1 ||
		available[0].GetSeatMapCapture() == nil {
		t.Fatalf("available capabilities = %v", available)
	}
	if !retryable(io.ErrUnexpectedEOF) || retryable(ErrUnauthorized) || retryable(ErrLeaseExpired) ||
		!retryable(&APIError{StatusCode: 500}) || retryable(&APIError{StatusCode: 400}) {
		t.Fatal("retry classification mismatch")
	}
	if delay, err := runtime.randomDuration(time.Second, time.Second); err != nil || delay != time.Second {
		t.Fatalf("fixed random delay = %v, %v", delay, err)
	}
	runtime.random = errorReader{}
	if _, err := runtime.randomDuration(time.Second, 2*time.Second); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("random delay error = %v", err)
	}
	if _, err := runtime.newRunID(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("run id error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitContext(cancelled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	if err := waitContext(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRegistrationAndLoopFailurePaths(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	runtime := newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	if err := runtime.Run(cancelled); err != nil {
		t.Fatalf("pre-cancelled runtime = %v", err)
	}
	runtime.credentials = &credentialErrorSource{}
	if _, _, err := runtime.register(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("credential error = %v", err)
	}
	if err := runtime.Run(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("runtime credential error = %v", err)
	}
	api := &fakeAPI{useRegisterResponse: true, registerResponse: &probepb.RegisterResponse{}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if _, _, err := runtime.register(context.Background()); err == nil {
		t.Fatal("invalid registration response accepted")
	}
	api = &fakeAPI{registerErrors: []error{&APIError{StatusCode: 400, Code: "invalid"}}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if _, _, err := runtime.register(context.Background()); err == nil {
		t.Fatal("non-retryable registration error ignored")
	}
	api = &fakeAPI{registerErrors: []error{io.ErrUnexpectedEOF}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.wait = func(context.Context, time.Duration) error { return context.Canceled }
	if _, _, err := runtime.register(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("registration wait error = %v", err)
	}
	api = &fakeAPI{heartbeatErrors: []error{io.ErrUnexpectedEOF}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.wait = func(context.Context, time.Duration) error { return nil }
	if err := runtime.sendInitialHeartbeat(context.Background(), Session{}); err != nil || api.heartbeatCalls != 2 {
		t.Fatalf("initial heartbeat retry = %v, calls = %d", err, api.heartbeatCalls)
	}
	api = &fakeAPI{heartbeatErrors: []error{io.ErrUnexpectedEOF}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.wait = func(context.Context, time.Duration) error { return context.Canceled }
	if err := runtime.sendInitialHeartbeat(context.Background(), Session{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("initial heartbeat wait error = %v", err)
	}
	api = &fakeAPI{heartbeatErrors: []error{ErrUnauthorized}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if err := runtime.sendInitialHeartbeat(context.Background(), Session{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("initial heartbeat terminal error = %v", err)
	}
	terminalHeartbeat := &APIError{StatusCode: 400, Code: "invalid"}
	api = &fakeAPI{heartbeatErrors: []error{terminalHeartbeat}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if err := runtime.Run(context.Background()); !errors.Is(err, terminalHeartbeat) || api.disconnectCalls != 1 {
		t.Fatalf("runtime terminal heartbeat = %v, disconnects = %d", err, api.disconnectCalls)
	}

	api = &fakeAPI{heartbeatErrors: []error{io.ErrUnexpectedEOF, ErrUnauthorized}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if err := runtime.heartbeatLoop(context.Background(), Session{}, time.Millisecond); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("heartbeat loop terminal error = %v", err)
	}
	api = &fakeAPI{heartbeatErrors: []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if err := runtime.heartbeatLoop(context.Background(), Session{}, time.Millisecond); !errors.Is(err, ErrHeartbeatUnavailable) {
		t.Fatalf("heartbeat failure limit error = %v", err)
	}
	api = &fakeAPI{heartbeatErrors: []error{io.ErrUnexpectedEOF, nil, ErrUnauthorized}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	if err := runtime.heartbeatLoop(context.Background(), Session{}, time.Millisecond); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("successful heartbeat did not reset failure count: %v", err)
	}
	loopContext, stopLoop := context.WithCancel(context.Background())
	stopLoop()
	if err := runtime.heartbeatLoop(loopContext, Session{}, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled heartbeat loop = %v", err)
	}

	runtime = newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	runtime.SetDraining(true)
	runtime.wait = func(context.Context, time.Duration) error { return context.Canceled }
	if err := runtime.workLoop(context.Background(), Session{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("draining work loop = %v", err)
	}
	for _, claimErr := range []error{ErrUnauthorized, &APIError{StatusCode: 400, Code: "invalid"}} {
		api = &fakeAPI{claimErrors: []error{claimErr}}
		runtime = newRuntimeForTest(t, api, &fakeExecutor{})
		if err := runtime.workLoop(context.Background(), Session{}); !errors.Is(err, claimErr) {
			t.Fatalf("claim error %v became %v", claimErr, err)
		}
	}
	api = &fakeAPI{claimErrors: []error{io.ErrUnexpectedEOF}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.wait = func(context.Context, time.Duration) error { return context.Canceled }
	if err := runtime.workLoop(context.Background(), Session{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("retryable claim loop = %v", err)
	}

	api = &fakeAPI{claimErrors: []error{&APIError{StatusCode: 400, Code: "stop"}}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.SetDraining(true)
	runtime.wait = func(context.Context, time.Duration) error {
		runtime.SetDraining(false)
		return nil
	}
	if err := runtime.workLoop(context.Background(), Session{}); err == nil {
		t.Fatal("drain continuation did not resume claiming")
	}

	assignmentTime := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	assignment := testAssignmentLease("assignment", "lease", assignmentTime.Add(30*time.Millisecond), assignmentTime.Add(time.Minute), testAssignmentTask())
	api = &fakeAPI{assignment: claimResponse(assignment), assignmentHeartbeatErrors: []error{ErrUnauthorized}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{wait: make(chan struct{})})
	runtime.clock = func() time.Time { return assignmentTime }
	if err := runtime.workLoop(context.Background(), Session{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("assignment authorization error = %v", err)
	}

	assignment.SetLeaseExpiresAt(timestamppb.New(time.Now().Add(time.Minute)))
	assignment.SetDeadline(timestamppb.New(time.Now().Add(time.Minute)))
	api = &fakeAPI{
		assignment:   claimResponse(assignment),
		commitErrors: []error{&APIError{StatusCode: 400, Code: "invalid_result"}},
	}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{captures: []*observationpb.Capture{capture(true)}})
	runtime.wait = func(context.Context, time.Duration) error { return context.Canceled }
	if err := runtime.workLoop(context.Background(), Session{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("assignment failure continuation = %v", err)
	}

	runtime = newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	runtime.config.ClaimMaximum = 2 * time.Millisecond
	runtime.random = errorReader{}
	if err := runtime.workLoop(context.Background(), Session{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("claim delay random error = %v", err)
	}
}

func TestRuntimeEnforcesCentralMinimumPolicy(t *testing.T) {
	t.Parallel()
	heartbeatResponse := &probepb.HeartbeatResponse{}
	heartbeatResponse.SetMinimumRuntimeVersion("2.0.0")
	heartbeatResponse.SetMinimumBrowserRevision("1228")
	api := &fakeAPI{heartbeatResponse: heartbeatResponse}
	runtime := newRuntimeForTest(t, api, &fakeExecutor{})
	runtime.config.Registration.GetRuntime().SetComponentVersion("1.9.9")
	runtime.config.Registration.GetRuntime().SetBrowserRevision("1228")
	if err := runtime.sendProbeHeartbeat(context.Background(), Session{}); !errors.Is(err, ErrIncompatibleRuntime) {
		t.Fatalf("outdated runtime policy error = %v", err)
	}
	if !runtime.draining() {
		t.Fatal("outdated runtime did not enter drain state")
	}
	runtime.remoteDrain = false
	runtime.config.Registration.GetRuntime().SetComponentVersion("2.0.0")
	runtime.config.Registration.GetRuntime().SetBrowserRevision("1227")
	if err := runtime.validateMinimumPolicy(heartbeatResponse); !errors.Is(err, ErrIncompatibleRuntime) {
		t.Fatalf("outdated browser policy error = %v", err)
	}
	for _, test := range []struct {
		current string
		minimum string
		want    bool
	}{
		{current: "2.1.3", minimum: "2.1.3", want: true},
		{current: "v2.2.0", minimum: "2.1.9", want: true},
		{current: "2.1.0-dev", minimum: "2.0.9", want: true},
		{current: "2.1.0-dev", minimum: "2.1.0", want: false},
		{current: "2.1.0-rc.10", minimum: "2.1.0-rc.2", want: true},
		{current: "2.1.0", minimum: "2.1.0-rc.10", want: true},
	} {
		got, err := semanticVersionAtLeast(test.current, test.minimum)
		if err != nil || got != test.want {
			t.Fatalf("semanticVersionAtLeast(%q, %q) = %t, %v", test.current, test.minimum, got, err)
		}
	}
	for _, test := range []struct {
		current string
		minimum string
		want    bool
	}{
		{current: "1228", minimum: "1228", want: true},
		{current: "1228", minimum: "1229", want: false},
		{current: "1229", minimum: "1228", want: true},
	} {
		got, err := browserRevisionAtLeast(test.current, test.minimum)
		if err != nil || got != test.want {
			t.Fatalf("browserRevisionAtLeast(%q, %q) = %t, %v", test.current, test.minimum, got, err)
		}
	}
	for _, test := range []struct {
		current string
		minimum string
	}{
		{current: "not-a-version", minimum: "2.0.0"},
		{current: "2.0.0", minimum: "not-a-version"},
		{current: "2..1", minimum: "2.1.0"},
	} {
		if _, err := semanticVersionAtLeast(test.current, test.minimum); err == nil {
			t.Fatalf("invalid semantic version accepted: %+v", test)
		}
	}
	for _, test := range []struct {
		current string
		minimum string
	}{
		{current: "-1", minimum: "1"},
		{current: "1228.0", minimum: "1228"},
		{current: "1228", minimum: "+1228"},
	} {
		if _, err := browserRevisionAtLeast(test.current, test.minimum); err == nil {
			t.Fatalf("invalid browser revision accepted: %+v", test)
		}
	}
	for _, value := range []string{"", " \t\n"} {
		if _, err := parseBrowserRevision(value); err == nil {
			t.Fatalf("blank browser revision %q accepted", value)
		}
	}
}

func TestRuntimeWorkLoopSkipsDelayAfterAvailabilityWaitingClaim(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseAPI := &fakeAPI{onClaim: cancel}
	api := availabilityWaitingAPI{fakeAPI: baseAPI}
	runtime := newRuntimeForTest(t, &api, &fakeExecutor{})
	// A long-polling claim owns the wait. If workLoop adds its random delay,
	// this reader makes the test fail before the cancellation is observed.
	runtime.random = errorReader{}
	if err := runtime.workLoop(ctx, Session{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("availability-waiting claim loop = %v", err)
	}
	if baseAPI.claimCalls != 1 {
		t.Fatalf("availability-waiting claim calls = %d", baseAPI.claimCalls)
	}
}

func TestRuntimeLeaseHeartbeatFailurePaths(t *testing.T) {
	t.Parallel()
	now := time.Now()
	base := func() *probepb.AssignmentLease {
		return testAssignmentLease("assignment", "lease", now.Add(30*time.Millisecond), now.Add(time.Minute), testAssignmentTask())
	}
	for _, heartbeatErr := range []error{ErrUnauthorized, ErrLeaseExpired} {
		api := &fakeAPI{assignmentHeartbeatErrors: []error{heartbeatErr}}
		runtime := newRuntimeForTest(t, api, &fakeExecutor{wait: make(chan struct{})})
		runtime.clock = func() time.Time { return now }
		if err := runtime.executeAssignment(context.Background(), Session{}, base()); !errors.Is(err, heartbeatErr) {
			t.Fatalf("lease heartbeat error %v became %v", heartbeatErr, err)
		}
	}
	api := &fakeAPI{
		assignmentHeartbeatErrors:   []error{io.ErrUnexpectedEOF},
		assignmentHeartbeatResponse: heartbeatAssignmentResponse(time.Now().Add(time.Minute)),
	}
	release := make(chan struct{})
	api.onAssignmentHeartbeat = func() { close(release) }
	runtime := newRuntimeForTest(t, api, &fakeExecutor{wait: release, captures: []*observationpb.Capture{capture(true)}})
	runtime.clock = func() time.Time { return now }
	if err := runtime.executeAssignment(context.Background(), Session{}, base()); err != nil {
		t.Fatalf("transient lease heartbeat = %v", err)
	}
	api = &fakeAPI{assignmentHeartbeatResponse: heartbeatAssignmentResponse(time.Now().Add(-time.Second))}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{wait: make(chan struct{})})
	runtime.clock = func() time.Time { return now }
	if err := runtime.executeAssignment(context.Background(), Session{}, base()); err == nil {
		t.Fatal("invalid lease extension accepted")
	}
	expiring := base()
	expiring.SetLeaseExpiresAt(timestamppb.New(time.Now().Add(20 * time.Millisecond)))
	runtime = newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{wait: make(chan struct{})})
	runtime.leaseHeartbeatMinimum = 40 * time.Millisecond
	if err := runtime.executeAssignment(context.Background(), Session{}, expiring); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("locally expired lease error = %v", err)
	}
	captureContext, cancelCapture := context.WithCancel(context.Background())
	cancelCapture()
	releaseCapture := make(chan struct{})
	runtime = newRuntimeForTest(t, &fakeAPI{}, contextIgnoringExecutor{wait: releaseCapture})
	validAssignment := base()
	validAssignment.SetLeaseExpiresAt(timestamppb.New(time.Now().Add(time.Minute)))
	validAssignment.SetDeadline(timestamppb.New(time.Now().Add(time.Minute)))
	if err := runtime.executeAssignment(captureContext, Session{}, validAssignment); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture error = %v", err)
	}
	close(releaseCapture)
	runtime = newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{captures: []*observationpb.Capture{capture(true)}})
	runtime.random = errorReader{}
	if err := runtime.executeAssignment(context.Background(), Session{}, validAssignment); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("result id failure = %v", err)
	}
}

func TestRuntimeCommitDeadlineAndWaitFailures(t *testing.T) {
	t.Parallel()
	result := &observationpb.AssignmentResult{}
	result.SetRunId("run")
	result.SetStartedAt(timestamppb.Now())
	result.SetFinishedAt(timestamppb.Now())
	result.SetFailed(&observationpb.Failed{})
	assignment := testAssignmentLease("assignment", "lease", time.Now().Add(time.Millisecond), time.Now().Add(time.Minute), testAssignmentTask())
	runtime := newRuntimeForTest(t, &fakeAPI{commitErrors: []error{io.ErrUnexpectedEOF}}, &fakeExecutor{})
	if err := runtime.commitResult(context.Background(), Session{}, assignment, result); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("commit deadline error = %v", err)
	}
	assignment.SetLeaseExpiresAt(timestamppb.New(time.Now().Add(time.Minute)))
	runtime = newRuntimeForTest(t, &fakeAPI{commitErrors: []error{io.ErrUnexpectedEOF}}, &fakeExecutor{})
	runtime.wait = func(context.Context, time.Duration) error { return context.Canceled }
	if err := runtime.commitResult(context.Background(), Session{}, assignment, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit wait error = %v", err)
	}
	runtime = newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	runtime.random = strings.NewReader(strings.Repeat("\x00", 16))
	delay, err := runtime.randomDuration(time.Second, 2*time.Second)
	if err != nil || delay < time.Second || delay > 2*time.Second {
		t.Fatalf("random delay = %v, %v", delay, err)
	}
}

func testAssignmentLease(assignmentID, leaseToken string, leaseExpiresAt, deadline time.Time, task *observationpb.AssignmentTask) *probepb.AssignmentLease {
	assignment := &probepb.AssignmentLease{}
	assignment.SetAssignmentId(assignmentID)
	assignment.SetLeaseToken(leaseToken)
	assignment.SetLeaseExpiresAt(timestamppb.New(leaseExpiresAt))
	assignment.SetDeadline(timestamppb.New(deadline))
	assignment.SetTask(task)
	return assignment
}

func claimResponse(assignment *probepb.AssignmentLease) *probepb.ClaimAssignmentResponse {
	response := &probepb.ClaimAssignmentResponse{}
	response.SetAssignment(assignment)
	return response
}

func heartbeatAssignmentResponse(leaseExpiresAt time.Time) *probepb.HeartbeatAssignmentResponse {
	response := &probepb.HeartbeatAssignmentResponse{}
	response.SetLeaseExpiresAt(timestamppb.New(leaseExpiresAt))
	return response
}

func capture(complete bool) *observationpb.Capture {
	value := &observationpb.Capture{}
	value.SetComplete(complete)
	value.SetObservedAt(timestamppb.Now())
	return value
}

func resultForOutput(output executionOutput) *observationpb.AssignmentResult {
	result := &observationpb.AssignmentResult{}
	completed := &observationpb.Completed{}
	switch {
	case output.liveSeat != nil:
		completed.SetLiveSeat(output.liveSeat)
	case output.catalog != nil:
		completed.SetCatalog(output.catalog)
	case len(output.captures) > 0:
		schedule := &observationpb.ScheduleCaptures{}
		schedule.SetCaptures(output.captures)
		completed.SetSchedule(schedule)
	}
	result.SetRunId("run-test")
	result.SetStartedAt(timestamppb.New(time.Unix(1, 0).UTC()))
	result.SetFinishedAt(timestamppb.New(time.Unix(2, 0).UTC()))
	result.SetCompleted(completed)
	return result
}

func catalogAssignmentTask() *observationpb.AssignmentTask {
	task := testAssignmentTask()
	setCatalogTask(task)
	return task
}

func catalogSnapshot(observedAt time.Time) *catalogpb.CatalogSnapshot {
	provider := &catalogpb.Provider{}
	provider.SetId("cgv")
	provider.SetName("CGV")
	snapshot := &catalogpb.CatalogSnapshot{}
	snapshot.SetProvider(provider)
	snapshot.SetObservedAt(timestamppb.New(observedAt))
	return snapshot
}

func newRuntimeForTest(t *testing.T, api API, executor Executor) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(api, StaticCredential("credential"), executor, Config{
		Registration: testRegistration(), ClaimMinimum: time.Millisecond, ClaimMaximum: time.Millisecond,
		ReconnectMinimum: time.Millisecond, ReconnectMaximum: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.leaseHeartbeatMinimum = time.Millisecond
	return runtime
}

type fakeExecutor struct {
	mu           sync.Mutex
	captures     []*observationpb.Capture
	catalog      *catalogpb.CatalogSnapshot
	liveSeat     *seatmappb.LiveSeatObservation
	err          error
	catalogErr   error
	wait         <-chan struct{}
	calls        int
	catalogCalls int
	seatMapCalls int
}

func (executor *fakeExecutor) CaptureSeatMap(
	context.Context,
	*observationpb.AssignmentTask,
) (*seatmappb.LiveSeatObservation, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.seatMapCalls++
	return executor.liveSeat, executor.err
}

type contextIgnoringExecutor struct {
	wait <-chan struct{}
}

func (executor contextIgnoringExecutor) Capture(context.Context, *observationpb.AssignmentTask) ([]*observationpb.Capture, error) {
	<-executor.wait
	return nil, nil
}

func (executor *fakeExecutor) Capture(ctx context.Context, _ *observationpb.AssignmentTask) ([]*observationpb.Capture, error) {
	executor.mu.Lock()
	executor.calls++
	executor.mu.Unlock()
	if executor.wait != nil {
		select {
		case <-executor.wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return executor.captures, executor.err
}

func (executor *fakeExecutor) CaptureCatalog(
	context.Context,
	*observationpb.AssignmentTask,
) (*catalogpb.CatalogSnapshot, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.catalogCalls++
	return executor.catalog, executor.catalogErr
}

type fakeAPI struct {
	mu                          sync.Mutex
	registerErrors              []error
	heartbeatErrors             []error
	claimErrors                 []error
	assignmentHeartbeatErrors   []error
	commitErrors                []error
	registerResponse            *probepb.RegisterResponse
	heartbeatResponse           *probepb.HeartbeatResponse
	useRegisterResponse         bool
	assignmentHeartbeatResponse *probepb.HeartbeatAssignmentResponse
	assignment                  *probepb.ClaimAssignmentResponse
	registerCalls               int
	heartbeatCalls              int
	claimCalls                  int
	assignmentHeartbeatCalls    int
	commitCalls                 int
	disconnectCalls             int
	committed                   *observationpb.AssignmentResult
	onCommit                    func()
	onClaim                     func()
	onAssignmentHeartbeat       func()
	cancelOnHeartbeat           func()
}

type availabilityWaitingAPI struct {
	*fakeAPI
}

func (availabilityWaitingAPI) AssignmentClaimWaitsForAvailability() bool { return true }

func (api *fakeAPI) Register(
	context.Context,
	string,
	*probepb.RegisterRequest,
) (*probepb.RegisterResponse, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.registerCalls++
	if err := shiftError(&api.registerErrors); err != nil {
		return nil, err
	}
	if api.useRegisterResponse {
		return api.registerResponse, nil
	}
	response := &probepb.RegisterResponse{}
	response.SetProbeId("probe")
	response.SetAccessToken("access")
	response.SetHeartbeatIntervalSeconds(1)
	return response, nil
}

func (api *fakeAPI) HeartbeatProbe(
	context.Context,
	Session,
	*probepb.HeartbeatRequest,
) (*probepb.HeartbeatResponse, error) {
	api.mu.Lock()
	api.heartbeatCalls++
	err := shiftError(&api.heartbeatErrors)
	cancel := api.cancelOnHeartbeat
	if api.registerCalls < 2 {
		cancel = nil
	}
	api.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if api.heartbeatResponse == nil {
		return &probepb.HeartbeatResponse{}, err
	}
	return api.heartbeatResponse, err
}

func (api *fakeAPI) DisconnectProbe(context.Context, Session) error {
	api.mu.Lock()
	api.disconnectCalls++
	api.mu.Unlock()
	return nil
}

func (api *fakeAPI) ClaimAssignment(context.Context, Session) (*probepb.ClaimAssignmentResponse, error) {
	api.mu.Lock()
	api.claimCalls++
	if err := shiftError(&api.claimErrors); err != nil {
		api.mu.Unlock()
		return nil, err
	}
	assignment := api.assignment
	api.assignment = nil
	onClaim := api.onClaim
	api.onClaim = nil
	api.mu.Unlock()
	if onClaim != nil {
		onClaim()
	}
	return assignment, nil
}

func (api *fakeAPI) HeartbeatAssignment(
	context.Context,
	Session,
	*probepb.AssignmentLease,
) (*probepb.HeartbeatAssignmentResponse, error) {
	api.mu.Lock()
	api.assignmentHeartbeatCalls++
	err := shiftError(&api.assignmentHeartbeatErrors)
	callback := api.onAssignmentHeartbeat
	api.onAssignmentHeartbeat = nil
	api.mu.Unlock()
	if callback != nil {
		callback()
	}
	if err != nil {
		return nil, err
	}
	if api.assignmentHeartbeatResponse != nil && api.assignmentHeartbeatResponse.GetLeaseExpiresAt() != nil {
		return api.assignmentHeartbeatResponse, nil
	}
	response := &probepb.HeartbeatAssignmentResponse{}
	response.SetLeaseExpiresAt(timestamppb.New(time.Now().Add(time.Minute)))
	return response, nil
}

func (api *fakeAPI) CommitResult(
	_ context.Context,
	_ Session,
	_ *probepb.AssignmentLease,
	result *observationpb.AssignmentResult,
) (*observationpb.ResultReceipt, error) {
	api.mu.Lock()
	api.commitCalls++
	api.committed = result
	err := shiftError(&api.commitErrors)
	callback := api.onCommit
	api.mu.Unlock()
	if callback != nil {
		callback()
	}
	return &observationpb.ResultReceipt{}, err
}

func shiftError(values *[]error) error {
	if len(*values) == 0 {
		return nil
	}
	value := (*values)[0]
	*values = (*values)[1:]
	return value
}
