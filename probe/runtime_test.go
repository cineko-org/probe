package probe

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	central "github.com/cineko-org/contracts/v3"
)

func TestRuntimeProcessesAssignmentAndDisconnects(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeAPI{}
	api.assignment = &central.ClaimAssignmentResponse{
		AssignmentID: "assignment_01", LeaseToken: "lease_01",
		LeaseExpiresAt: time.Now().Add(time.Minute), Deadline: time.Now().Add(2 * time.Minute),
		Task: testAssignmentTask(),
	}
	api.onCommit = cancel
	executor := &fakeExecutor{captures: []central.Capture{{
		TargetDate: "2026-08-20", Complete: true, ObservedAt: time.Now(), Showtimes: []central.Showtime{},
	}}}
	runtime := newRuntimeForTest(t, api, executor)
	if err := runtime.Run(ctx); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.registerCalls != 1 || api.claimCalls == 0 || api.commitCalls != 1 || api.disconnectCalls != 1 ||
		api.committed.Status != "completed" || api.committed.RunID == "" || executor.calls != 1 {
		t.Fatalf("API = %+v, result = %+v, executor calls = %d", api, api.committed, executor.calls)
	}
}

func TestRuntimeReportsPartialAssignment(t *testing.T) {
	t.Parallel()
	now := time.Now()
	api := &fakeAPI{}
	runtime := newRuntimeForTest(t, api, &fakeExecutor{captures: []central.Capture{
		{TargetDate: "2026-08-20", Complete: true, ObservedAt: now},
		{TargetDate: "2026-08-21", Complete: false, ObservedAt: now},
	}})
	assignment := central.ClaimAssignmentResponse{
		AssignmentID: "assignment_partial", LeaseToken: "lease",
		LeaseExpiresAt: now.Add(time.Minute), Deadline: now.Add(2 * time.Minute),
		Task: testAssignmentTask(),
	}
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	if api.committed.Status != "partial" {
		t.Fatalf("result status = %q", api.committed.Status)
	}
}

func TestRuntimeProcessesCatalogAssignmentAndRejectsUnsupportedExecutor(t *testing.T) {
	t.Parallel()
	now := time.Now()
	assignment := central.ClaimAssignmentResponse{
		AssignmentID: "catalog_assignment", LeaseToken: "lease",
		LeaseExpiresAt: now.Add(time.Minute), Deadline: now.Add(2 * time.Minute),
		Task: testAssignmentTask(),
	}
	assignment.Task.Kind = central.CapabilityCGVCatalogCapture
	catalog := &central.CatalogSnapshot{
		Provider: central.Provider{ID: central.ProviderCGV, Name: "CGV"}, ObservedAt: now,
	}
	api := &fakeAPI{}
	executor := &fakeExecutor{catalog: catalog}
	runtime := newRuntimeForTest(t, api, executor)
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	committed := api.committed
	api.mu.Unlock()
	if committed.Status != "completed" || committed.Catalog != catalog || committed.Captures != nil ||
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
	if committed.Status != "failed" || committed.Catalog != nil {
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
	if committed.Status != "failed" || committed.Catalog != nil {
		t.Fatalf("failed catalog result = %+v", committed)
	}
}

func TestRuntimeProcessesSeatMapAssignment(t *testing.T) {
	t.Parallel()
	now := time.Now()
	assignment := central.ClaimAssignmentResponse{
		AssignmentID: "seat_assignment", LeaseToken: "lease",
		LeaseExpiresAt: now.Add(time.Minute), Deadline: now.Add(2 * time.Minute),
		Task: testAssignmentTask(),
	}
	assignment.Task.Kind = central.CapabilityCGVSeatMapCapture
	seatMap := &central.SeatMapVersion{ID: "seat_map", AuditoriumID: "auditorium", LayoutHash: "hash"}
	api := &fakeAPI{}
	executor := &fakeExecutor{seatMap: seatMap}
	runtime := newRuntimeForTest(t, api, executor)
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	if api.committed.Status != "completed" || api.committed.SeatMap != seatMap || executor.seatMapCalls != 1 {
		t.Fatalf("seat-map result = %+v, calls = %d", api.committed, executor.seatMapCalls)
	}

	api = &fakeAPI{}
	runtime = newRuntimeForTest(t, api, contextIgnoringExecutor{wait: make(chan struct{})})
	if err := runtime.executeAssignment(context.Background(), Session{}, assignment); err != nil {
		t.Fatal(err)
	}
	if api.committed.Status != "failed" || api.committed.SeatMap != nil {
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
	executor := &fakeExecutor{wait: release, captures: []central.Capture{{TargetDate: "2026-08-20", Complete: true}}}
	runtime := newRuntimeForTest(t, api, executor)
	assignment := central.ClaimAssignmentResponse{
		AssignmentID: "assignment", LeaseToken: "lease",
		LeaseExpiresAt: time.Now().Add(3 * time.Second), Deadline: time.Now().Add(time.Minute),
		Task: testAssignmentTask(),
	}
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
	api := &fakeAPI{assignment: &central.ClaimAssignmentResponse{
		AssignmentID: "assignment_during_drain", LeaseToken: "lease",
		LeaseExpiresAt: time.Now().Add(time.Minute), Deadline: time.Now().Add(time.Minute),
		Task: testAssignmentTask(),
	}}
	runtime := newRuntimeForTest(t, api, executor)
	api.onClaim = func() { runtime.SetDraining(true) }
	api.onCommit = cancel
	if err := runtime.workLoop(ctx, Session{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("workLoop() error = %v", err)
	}
	runtime.random = errorReader{}
	if err := runtime.rejectClaimWhileDraining(context.Background(), Session{}, central.ClaimAssignmentResponse{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("drained claim run ID error = %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if executor.calls != 0 || api.commitCalls != 1 {
		t.Fatalf("executor calls = %d, commits = %d", executor.calls, api.commitCalls)
	}
	if api.committed.Status != "failed" || api.committed.RunID == "" ||
		api.committed.StartedAt.IsZero() || api.committed.FinishedAt.IsZero() ||
		api.committed.Captures == nil || len(api.committed.Captures) != 0 {
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
	assignment := central.ClaimAssignmentResponse{
		AssignmentID: "assignment", LeaseToken: "lease", LeaseExpiresAt: now.Add(time.Minute),
		Deadline: now.Add(time.Minute), Task: testAssignmentTask(),
	}
	for _, expired := range []central.ClaimAssignmentResponse{
		func() central.ClaimAssignmentResponse {
			value := assignment
			value.LeaseExpiresAt = now.Add(-time.Second)
			return value
		}(),
		func() central.ClaimAssignmentResponse {
			value := assignment
			value.Deadline = now.Add(-time.Second)
			return value
		}(),
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
	if err != nil || result.Status != "failed" {
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
	invalidConcurrency := valid
	invalidConcurrency.Registration.MaxConcurrency = 2
	if _, err := NewRuntime(&fakeAPI{}, StaticCredential("token"), &fakeExecutor{}, invalidConcurrency); err == nil {
		t.Fatal("invalid concurrency accepted")
	}
	invalidKind := valid
	invalidKind.Registration.Kind = "invalid"
	if _, err := NewRuntime(&fakeAPI{}, StaticCredential("token"), &fakeExecutor{}, invalidKind); err == nil {
		t.Fatal("invalid kind accepted")
	}
	invalidIntervals := valid
	invalidIntervals.ClaimMinimum = 2 * time.Second
	invalidIntervals.ClaimMaximum = time.Second
	if _, err := NewRuntime(&fakeAPI{}, StaticCredential("token"), &fakeExecutor{}, invalidIntervals); err == nil {
		t.Fatal("invalid intervals accepted")
	}
	runtime := newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	if err := runtime.Run(nil); err == nil { //nolint:staticcheck // Explicitly verifies the nil-context boundary.
		t.Fatal("nil runtime context accepted")
	}
	runtime.SetDraining(true)
	state := runtime.heartbeatState()
	if !state.Draining || state.AvailableSlots != 0 {
		t.Fatalf("draining heartbeat = %+v", state)
	}
	runtime.SetDraining(false)
	runtime.setActive("assignment")
	state = runtime.heartbeatState()
	if state.AvailableSlots != 0 || len(state.ActiveAssignmentIDs) != 1 {
		t.Fatalf("active heartbeat = %+v", state)
	}
	runtime.setActive("")
	runtime.remoteDrain = true
	if !runtime.draining() {
		t.Fatal("remote drain was ignored")
	}
	if resultStatus(executionOutput{}, nil) != "failed" ||
		resultStatus(executionOutput{catalog: &central.CatalogSnapshot{}}, nil) != "completed" ||
		resultStatus(executionOutput{seatMap: &central.SeatMapVersion{}}, nil) != "completed" ||
		resultStatus(executionOutput{captures: []central.Capture{{Complete: true}, {Complete: false}}}, nil) != "partial" ||
		resultStatus(executionOutput{captures: []central.Capture{{Complete: false}}}, nil) != "failed" ||
		resultStatus(executionOutput{captures: []central.Capture{{Complete: true}}}, nil) != "completed" {
		t.Fatal("result status classification mismatch")
	}
	runtime.config.Registration.Capabilities = []string{
		central.CapabilityCGVScheduleCapture, central.CapabilityCGVSeatMapCapture,
	}
	runtime.config.AvailableCapabilities = func() []string {
		return []string{central.CapabilityCGVSeatMapCapture, "unsupported", central.CapabilityCGVSeatMapCapture}
	}
	if available := runtime.availableCapabilities(); len(available) != 1 ||
		available[0] != central.CapabilityCGVSeatMapCapture {
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
	api := &fakeAPI{useRegisterResponse: true, registerResponse: central.RegisterProbeResponse{}}
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
	assignment := central.ClaimAssignmentResponse{
		AssignmentID: "assignment", LeaseToken: "lease", LeaseExpiresAt: assignmentTime.Add(30 * time.Millisecond),
		Deadline: assignmentTime.Add(time.Minute), Task: testAssignmentTask(),
	}
	api = &fakeAPI{assignment: &assignment, assignmentHeartbeatErrors: []error{ErrUnauthorized}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{wait: make(chan struct{})})
	runtime.clock = func() time.Time { return assignmentTime }
	if err := runtime.workLoop(context.Background(), Session{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("assignment authorization error = %v", err)
	}

	assignment.LeaseExpiresAt = time.Now().Add(time.Minute)
	assignment.Deadline = time.Now().Add(time.Minute)
	api = &fakeAPI{
		assignment:   &assignment,
		commitErrors: []error{&APIError{StatusCode: 400, Code: "invalid_result"}},
	}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{captures: []central.Capture{{Complete: true}}})
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

func TestRuntimeLeaseHeartbeatFailurePaths(t *testing.T) {
	t.Parallel()
	now := time.Now()
	base := func() central.ClaimAssignmentResponse {
		return central.ClaimAssignmentResponse{
			AssignmentID: "assignment", LeaseToken: "lease", LeaseExpiresAt: now.Add(30 * time.Millisecond),
			Deadline: now.Add(time.Minute), Task: testAssignmentTask(),
		}
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
		assignmentHeartbeatResponse: central.AssignmentHeartbeatResponse{LeaseExpiresAt: time.Now().Add(time.Minute)},
	}
	release := make(chan struct{})
	api.onAssignmentHeartbeat = func() { close(release) }
	runtime := newRuntimeForTest(t, api, &fakeExecutor{wait: release, captures: []central.Capture{{Complete: true}}})
	runtime.clock = func() time.Time { return now }
	if err := runtime.executeAssignment(context.Background(), Session{}, base()); err != nil {
		t.Fatalf("transient lease heartbeat = %v", err)
	}
	api = &fakeAPI{assignmentHeartbeatResponse: central.AssignmentHeartbeatResponse{LeaseExpiresAt: time.Now().Add(-time.Second)}}
	runtime = newRuntimeForTest(t, api, &fakeExecutor{wait: make(chan struct{})})
	runtime.clock = func() time.Time { return now }
	if err := runtime.executeAssignment(context.Background(), Session{}, base()); err == nil {
		t.Fatal("invalid lease extension accepted")
	}
	expiring := base()
	expiring.LeaseExpiresAt = time.Now().Add(20 * time.Millisecond)
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
	validAssignment.LeaseExpiresAt = time.Now().Add(time.Minute)
	validAssignment.Deadline = time.Now().Add(time.Minute)
	if err := runtime.executeAssignment(captureContext, Session{}, validAssignment); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture error = %v", err)
	}
	close(releaseCapture)
	runtime = newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{captures: []central.Capture{{Complete: true}}})
	runtime.random = errorReader{}
	if err := runtime.executeAssignment(context.Background(), Session{}, validAssignment); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("result id failure = %v", err)
	}
}

func TestRuntimeCommitDeadlineAndWaitFailures(t *testing.T) {
	t.Parallel()
	result := central.AssignmentResult{RunID: "run", Status: "failed", StartedAt: time.Now(), FinishedAt: time.Now()}
	assignment := central.ClaimAssignmentResponse{LeaseExpiresAt: time.Now().Add(time.Millisecond)}
	runtime := newRuntimeForTest(t, &fakeAPI{commitErrors: []error{io.ErrUnexpectedEOF}}, &fakeExecutor{})
	if err := runtime.commitResult(context.Background(), Session{}, assignment, result); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("commit deadline error = %v", err)
	}
	assignment.LeaseExpiresAt = time.Now().Add(time.Minute)
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
	captures     []central.Capture
	catalog      *central.CatalogSnapshot
	seatMap      *central.SeatMapVersion
	err          error
	catalogErr   error
	wait         <-chan struct{}
	calls        int
	catalogCalls int
	seatMapCalls int
}

func (executor *fakeExecutor) CaptureSeatMap(
	context.Context,
	central.AssignmentTask,
) (*central.SeatMapVersion, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.seatMapCalls++
	return executor.seatMap, executor.err
}

type contextIgnoringExecutor struct {
	wait <-chan struct{}
}

func (executor contextIgnoringExecutor) Capture(context.Context, central.AssignmentTask) ([]central.Capture, error) {
	<-executor.wait
	return nil, nil
}

func (executor *fakeExecutor) Capture(ctx context.Context, _ central.AssignmentTask) ([]central.Capture, error) {
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
	central.AssignmentTask,
) (*central.CatalogSnapshot, error) {
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
	registerResponse            central.RegisterProbeResponse
	useRegisterResponse         bool
	assignmentHeartbeatResponse central.AssignmentHeartbeatResponse
	assignment                  *central.ClaimAssignmentResponse
	registerCalls               int
	heartbeatCalls              int
	claimCalls                  int
	assignmentHeartbeatCalls    int
	commitCalls                 int
	disconnectCalls             int
	committed                   central.AssignmentResult
	onCommit                    func()
	onClaim                     func()
	onAssignmentHeartbeat       func()
	cancelOnHeartbeat           func()
}

func (api *fakeAPI) Register(
	context.Context,
	string,
	central.RegisterProbeRequest,
) (central.RegisterProbeResponse, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.registerCalls++
	if err := shiftError(&api.registerErrors); err != nil {
		return central.RegisterProbeResponse{}, err
	}
	if api.useRegisterResponse {
		return api.registerResponse, nil
	}
	return central.RegisterProbeResponse{
		ProbeID: "probe", AccessToken: "access", HeartbeatIntervalSeconds: 1,
	}, nil
}

func (api *fakeAPI) HeartbeatProbe(
	context.Context,
	Session,
	central.ProbeHeartbeatRequest,
) (central.ProbeHeartbeatResponse, error) {
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
	return central.ProbeHeartbeatResponse{}, err
}

func (api *fakeAPI) DisconnectProbe(context.Context, Session) error {
	api.mu.Lock()
	api.disconnectCalls++
	api.mu.Unlock()
	return nil
}

func (api *fakeAPI) ClaimAssignment(context.Context, Session) (*central.ClaimAssignmentResponse, error) {
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
	central.ClaimAssignmentResponse,
) (central.AssignmentHeartbeatResponse, error) {
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
		return central.AssignmentHeartbeatResponse{}, err
	}
	if !api.assignmentHeartbeatResponse.LeaseExpiresAt.IsZero() {
		return api.assignmentHeartbeatResponse, nil
	}
	return central.AssignmentHeartbeatResponse{LeaseExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (api *fakeAPI) CommitResult(
	_ context.Context,
	_ Session,
	_ central.ClaimAssignmentResponse,
	result central.AssignmentResult,
) (central.ResultReceipt, error) {
	api.mu.Lock()
	api.commitCalls++
	api.committed = result
	err := shiftError(&api.commitErrors)
	callback := api.onCommit
	api.mu.Unlock()
	if callback != nil {
		callback()
	}
	return central.ResultReceipt{}, err
}

func shiftError(values *[]error) error {
	if len(*values) == 0 {
		return nil
	}
	value := (*values)[0]
	*values = (*values)[1:]
	return value
}
