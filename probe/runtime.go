package probe

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"buf.build/go/protovalidate"
	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	collectionpb "github.com/cineko-org/contracts/v3/gen/go/cineko/collection"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	probepb "github.com/cineko-org/contracts/v3/gen/go/cineko/probe"
	seatmappb "github.com/cineko-org/contracts/v3/gen/go/cineko/seatmap"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	"github.com/cineko-org/probe/v2/internal/telemetry"

	"golang.org/x/mod/semver"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	DefaultClaimMinimum      = 2 * time.Second
	DefaultClaimMaximum      = 5 * time.Second
	DefaultReconnectMinimum  = time.Second
	DefaultReconnectMaximum  = 30 * time.Second
	DefaultHeartbeatFailures = 3
)

var (
	ErrHeartbeatUnavailable = errors.New("central heartbeat is unavailable")
	ErrIncompatibleRuntime  = errors.New("probe runtime does not meet Central minimum policy")
	errLocalExecution       = errors.New("probe local execution invariant failed")
)

type Executor interface {
	Capture(context.Context, *observationpb.AssignmentTask) ([]*observationpb.Capture, error)
}

type catalogExecutor interface {
	CaptureCatalog(context.Context, *observationpb.AssignmentTask) (*catalogpb.CatalogSnapshot, error)
}

type SeatMapExecutor interface {
	CaptureSeatMap(context.Context, *observationpb.AssignmentTask) (*seatmappb.LiveSeatObservation, error)
}

type SeatAvailabilityExecutor interface {
	CaptureSeatAvailability(context.Context, *observationpb.AssignmentTask) (*seatmappb.LiveSeatObservation, error)
}

type executionOutput struct {
	captures []*observationpb.Capture
	catalog  *catalogpb.CatalogSnapshot
	liveSeat *seatmappb.LiveSeatObservation
}

type captureResult struct {
	output executionOutput
	err    error
}

type Config struct {
	Registration          *probepb.RegisterRequest
	ClaimMinimum          time.Duration
	ClaimMaximum          time.Duration
	ReconnectMinimum      time.Duration
	ReconnectMaximum      time.Duration
	HeartbeatFailureLimit int
	AvailableCapabilities func() []*observationpb.Capability
	Logger                *slog.Logger
}

type Runtime struct {
	api                   API
	credentials           CredentialSource
	executor              Executor
	config                Config
	clock                 func() time.Time
	wait                  func(context.Context, time.Duration) error
	random                io.Reader
	leaseHeartbeatMinimum time.Duration

	mu          sync.Mutex
	activeID    string
	localDrain  bool
	remoteDrain bool
}

func NewRuntime(api API, credentials CredentialSource, executor Executor, config Config) (*Runtime, error) {
	if api == nil || credentials == nil || executor == nil {
		return nil, errors.New("probe API, credential source and executor are required")
	}
	if config.Registration == nil || config.Registration.GetMaxConcurrency() != 1 {
		return nil, errors.New("probe runtime currently requires maxConcurrency=1")
	}
	if config.Registration.GetKind().GetContainer() == nil && config.Registration.GetKind().GetClient() == nil {
		return nil, errors.New("probe runtime kind must be container or client")
	}
	defaultDuration(&config.ClaimMinimum, DefaultClaimMinimum)
	defaultDuration(&config.ClaimMaximum, DefaultClaimMaximum)
	defaultDuration(&config.ReconnectMinimum, DefaultReconnectMinimum)
	defaultDuration(&config.ReconnectMaximum, DefaultReconnectMaximum)
	if config.HeartbeatFailureLimit == 0 {
		config.HeartbeatFailureLimit = DefaultHeartbeatFailures
	}
	if config.ClaimMinimum <= 0 || config.ClaimMaximum < config.ClaimMinimum ||
		config.ReconnectMinimum <= 0 || config.ReconnectMaximum < config.ReconnectMinimum ||
		config.HeartbeatFailureLimit < 1 {
		return nil, errors.New("probe retry intervals are invalid")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runtime{
		api: api, credentials: credentials, executor: executor, config: config,
		clock: time.Now, wait: waitContext, random: rand.Reader, leaseHeartbeatMinimum: time.Second,
	}, nil
}

func (runtime *Runtime) SetDraining(draining bool) {
	runtime.mu.Lock()
	runtime.localDrain = draining
	runtime.mu.Unlock()
}

func (runtime *Runtime) Run(ctx context.Context) error {
	return runtime.run(ctx, nil)
}

func (runtime *Runtime) RunReady(ctx context.Context, ready chan<- error) error {
	if ready == nil {
		return errors.New("probe readiness channel is required")
	}
	return runtime.run(ctx, ready)
}

func (runtime *Runtime) run(ctx context.Context, ready chan<- error) error {
	if ctx == nil {
		err := errors.New("probe runtime context is required")
		notifyReady(ready, err)
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		session, heartbeatInterval, err := runtime.register(ctx)
		if err != nil {
			notifyReady(ready, err)
			return err
		}
		if err = runtime.sendInitialHeartbeat(ctx, session); err != nil {
			runtime.disconnect(session)
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			if errors.Is(err, ErrUnauthorized) {
				runtime.config.Logger.WarnContext(ctx, "Probe session expired before readiness",
					"domain", "probe", "event", "probe.session.expired", "outcome", "requeued",
					"reason", "session_expired_before_ready")
				continue
			}
			notifyReady(ready, err)
			return err
		}
		notifyReady(ready, nil)
		ready = nil
		err = runtime.runSession(ctx, session, heartbeatInterval)
		runtime.disconnect(session)
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if !errors.Is(err, ErrUnauthorized) {
			return err
		}
		runtime.config.Logger.WarnContext(ctx, "Probe session expired",
			"domain", "probe", "event", "probe.session.expired", "outcome", "requeued",
			"reason", "session_expired")
	}
}

func notifyReady(ready chan<- error, err error) {
	if ready != nil {
		ready <- err
	}
}

func (runtime *Runtime) register(ctx context.Context) (Session, time.Duration, error) {
	credential, err := runtime.credentials.Credential(ctx)
	if err != nil {
		return Session{}, 0, err
	}
	backoff := runtime.config.ReconnectMinimum
	for {
		response, err := runtime.api.Register(ctx, credential, runtime.config.Registration)
		if err == nil {
			if response == nil || response.GetProbeId() == "" || response.GetAccessToken() == "" || response.GetHeartbeatIntervalSeconds() <= 0 {
				return Session{}, 0, errors.New("central returned an invalid Probe registration")
			}
			return Session{ProbeID: response.GetProbeId(), AccessToken: response.GetAccessToken()},
				time.Duration(response.GetHeartbeatIntervalSeconds()) * time.Second, nil
		}
		if !retryable(err) {
			return Session{}, 0, err
		}
		runtime.logRetry("register", err, backoff)
		if err := runtime.wait(ctx, backoff); err != nil {
			return Session{}, 0, err
		}
		backoff = min(backoff*2, runtime.config.ReconnectMaximum)
	}
}

func (runtime *Runtime) runSession(
	ctx context.Context,
	session Session,
	heartbeatInterval time.Duration,
) error {
	group, groupContext := errgroup.WithContext(ctx)
	group.Go(func() error { return runtime.heartbeatLoop(groupContext, session, heartbeatInterval) })
	group.Go(func() error { return runtime.workLoop(groupContext, session) })
	return group.Wait()
}

func (runtime *Runtime) sendInitialHeartbeat(ctx context.Context, session Session) error {
	backoff := runtime.config.ReconnectMinimum
	for {
		err := runtime.sendProbeHeartbeat(ctx, session)
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		runtime.logRetry("initial heartbeat", err, backoff)
		if err := runtime.wait(ctx, backoff); err != nil {
			return err
		}
		backoff = min(backoff*2, runtime.config.ReconnectMaximum)
	}
}

func (runtime *Runtime) heartbeatLoop(
	ctx context.Context,
	session Session,
	interval time.Duration,
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := runtime.sendProbeHeartbeat(ctx, session); err != nil {
				if !retryable(err) {
					return err
				}
				consecutiveFailures++
				runtime.config.Logger.WarnContext(ctx, "Probe heartbeat failed",
					"domain", "probe", "event", "probe.heartbeat.completed", "outcome", "failed",
					"reason", "central_request_failed", "error_type", telemetry.ErrorType(err))
				if consecutiveFailures >= runtime.config.HeartbeatFailureLimit {
					return fmt.Errorf("%w after %d consecutive failures: %w",
						ErrHeartbeatUnavailable, consecutiveFailures, err)
				}
				continue
			}
			consecutiveFailures = 0
		}
	}
}

func (runtime *Runtime) sendProbeHeartbeat(ctx context.Context, session Session) error {
	request := runtime.heartbeatState()
	response, err := runtime.api.HeartbeatProbe(ctx, session, request)
	if err != nil {
		return err
	}
	if err := runtime.validateMinimumPolicy(response); err != nil {
		runtime.mu.Lock()
		runtime.remoteDrain = true
		runtime.mu.Unlock()
		return err
	}
	runtime.mu.Lock()
	runtime.remoteDrain = response.GetDrain()
	runtime.mu.Unlock()
	return nil
}

func (runtime *Runtime) validateMinimumPolicy(response *probepb.HeartbeatResponse) error {
	if response == nil || runtime.config.Registration.GetRuntime() == nil {
		return errors.New("central returned an invalid Probe heartbeat")
	}
	runtimeVersion := runtime.config.Registration.GetRuntime().GetComponentVersion()
	browserRevision := runtime.config.Registration.GetRuntime().GetBrowserRevision()
	if ok, err := semanticVersionAtLeast(runtimeVersion, response.GetMinimumRuntimeVersion()); err != nil || !ok {
		return fmt.Errorf("%w: runtime %q, minimum %q", ErrIncompatibleRuntime, runtimeVersion, response.GetMinimumRuntimeVersion())
	}
	if ok, err := browserRevisionAtLeast(browserRevision, response.GetMinimumBrowserRevision()); err != nil || !ok {
		return fmt.Errorf("%w: browser %q, minimum %q", ErrIncompatibleRuntime, browserRevision, response.GetMinimumBrowserRevision())
	}
	return nil
}

func semanticVersionAtLeast(current, minimum string) (bool, error) {
	minimum = canonicalSemanticVersion(minimum)
	if minimum == "" {
		return true, nil
	}
	current = canonicalSemanticVersion(current)
	if !semver.IsValid(current) || !semver.IsValid(minimum) {
		return false, errors.New("runtime versions must use semantic versioning")
	}
	return semver.Compare(current, minimum) >= 0, nil
}

func canonicalSemanticVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "v") {
		return value
	}
	return "v" + value
}

func browserRevisionAtLeast(current, minimum string) (bool, error) {
	minimum = strings.TrimSpace(minimum)
	if minimum == "" {
		return true, nil
	}
	currentValue, err := parseBrowserRevision(current)
	if err != nil {
		return false, err
	}
	minimumValue, err := parseBrowserRevision(minimum)
	if err != nil {
		return false, err
	}
	return currentValue.Cmp(minimumValue) >= 0, nil
}

func parseBrowserRevision(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("browser revision is empty")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return nil, errors.New("browser revision must be a nonnegative integer")
		}
	}
	// Every rune is an ASCII digit, so base-10 parsing cannot fail.
	revision, _ := new(big.Int).SetString(value, 10)
	return revision, nil
}

func (runtime *Runtime) workLoop(ctx context.Context, session Session) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if runtime.draining() {
			if err := runtime.wait(ctx, runtime.config.ClaimMaximum); err != nil {
				return err
			}
			continue
		}
		assignment, err := runtime.api.ClaimAssignment(ctx, session)
		claimWaitsForAvailability := assignmentClaimWaitsForAvailability(runtime.api)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return err
			}
			if !retryable(err) {
				return err
			}
			runtime.config.Logger.WarnContext(ctx, "Probe assignment claim failed",
				"domain", "observation", "event", "observation.assignment.claim.completed", "outcome", "failed",
				"reason", "central_request_failed", "error_type", telemetry.ErrorType(err))
		} else if assignment != nil && assignment.GetAssignment() != nil {
			if err := runtime.handleClaimedAssignment(ctx, session, assignment.GetAssignment()); err != nil {
				return err
			}
		}
		if err == nil && claimWaitsForAvailability {
			continue
		}
		delay, delayErr := runtime.randomDuration(runtime.config.ClaimMinimum, runtime.config.ClaimMaximum)
		if delayErr != nil {
			return delayErr
		}
		if err := runtime.wait(ctx, delay); err != nil {
			return err
		}
	}
}

func assignmentClaimWaitsForAvailability(api API) bool {
	waiter, ok := api.(interface{ AssignmentClaimWaitsForAvailability() bool })
	return ok && waiter.AssignmentClaimWaitsForAvailability()
}

func (runtime *Runtime) handleClaimedAssignment(
	ctx context.Context,
	session Session,
	assignment *probepb.AssignmentLease,
) error {
	runtime.config.Logger.InfoContext(ctx, "Observation assignment claimed",
		"domain", "observation", "event", "observation.assignment.claimed", "outcome", "succeeded",
		"assignment_id", assignment.GetAssignmentId(), "task_kind", assignmentTaskKind(assignment.GetTask()))
	runtime.setActive(assignment.GetAssignmentId())
	defer runtime.setActive("")
	var err error
	if runtime.draining() {
		err = runtime.rejectClaimWhileDraining(ctx, session, assignment)
	} else {
		err = runtime.executeAssignment(ctx, session, assignment)
	}
	if errors.Is(err, ErrUnauthorized) {
		return err
	}
	if err != nil && !errors.Is(err, ErrLeaseExpired) && ctx.Err() == nil {
		runtime.config.Logger.ErrorContext(ctx, "Observation assignment failed",
			"domain", "observation", "event", "observation.assignment.completed", "outcome", "failed",
			"assignment_id", assignment.GetAssignmentId(), "task_kind", assignmentTaskKind(assignment.GetTask()),
			"reason", "execution_failed", "error_type", telemetry.ErrorType(err))
	}
	return nil
}

func (runtime *Runtime) rejectClaimWhileDraining(
	ctx context.Context,
	session Session,
	assignment *probepb.AssignmentLease,
) error {
	now := runtime.clock().UTC()
	result, err := runtime.assignmentResult(
		executionOutput{captures: []*observationpb.Capture{}},
		fmt.Errorf("%w: probe is draining", errLocalExecution), now, now,
	)
	if err != nil {
		return err
	}
	return runtime.commitResult(ctx, session, assignment, result)
}

func (runtime *Runtime) executeAssignment(
	ctx context.Context,
	session Session,
	assignment *probepb.AssignmentLease,
) error {
	if !timestampAfter(assignment.GetLeaseExpiresAt(), runtime.clock()) || !timestampAfter(assignment.GetDeadline(), runtime.clock()) {
		return ErrLeaseExpired
	}
	startedAt := runtime.clock().UTC()
	assignmentContext, cancel := context.WithCancel(ctx)
	defer cancel()
	resultChannel := make(chan captureResult, 1)
	go func() { resultChannel <- runtime.captureAssignment(assignmentContext, assignment.GetTask()) }()
	heartbeatInterval := max(
		timestampTime(assignment.GetLeaseExpiresAt()).Sub(runtime.clock())/3,
		runtime.leaseHeartbeatMinimum,
	)
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case capture := <-resultChannel:
			return runtime.commitCapturedResult(ctx, session, assignment, capture, startedAt)
		case <-ticker.C:
			if !timestampAfter(assignment.GetLeaseExpiresAt(), runtime.clock()) {
				cancel()
				return ErrLeaseExpired
			}
			response, err := runtime.api.HeartbeatAssignment(ctx, session, assignment)
			switch {
			case err != nil:
				if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrLeaseExpired) {
					cancel()
					return err
				}
				runtime.config.Logger.WarnContext(ctx, "Observation assignment heartbeat failed",
					"domain", "observation", "event", "observation.assignment.heartbeat.completed",
					"outcome", "failed", "assignment_id", assignment.GetAssignmentId(),
					"reason", "central_request_failed", "error_type", telemetry.ErrorType(err))
			case response == nil || !timestampAfter(response.GetLeaseExpiresAt(), runtime.clock()):
				cancel()
				return errors.New("central returned an invalid assignment lease extension")
			default:
				assignment.SetLeaseExpiresAt(response.GetLeaseExpiresAt())
			}
		}
	}
}

func (runtime *Runtime) commitCapturedResult(
	ctx context.Context,
	session Session,
	assignment *probepb.AssignmentLease,
	capture captureResult,
	startedAt time.Time,
) error {
	finishedAt := runtime.clock().UTC()
	result, err := runtime.assignmentResult(capture.output, capture.err, startedAt, finishedAt)
	if err != nil {
		return err
	}
	if err := runtime.commitResult(ctx, session, assignment, result); err != nil {
		return err
	}
	outcome := resultOutcome(result)
	runtime.config.Logger.InfoContext(ctx, "Observation assignment completed",
		"domain", "observation", "event", "observation.assignment.completed", "outcome", outcome,
		"assignment_id", assignment.GetAssignmentId(), "run_id", result.GetRunId(),
		"task_kind", assignmentTaskKind(assignment.GetTask()), "result_status", outcome,
		"duration_ms", finishedAt.Sub(startedAt).Milliseconds())
	return nil
}

func (runtime *Runtime) captureAssignment(ctx context.Context, task *observationpb.AssignmentTask) captureResult {
	if task == nil {
		return captureResult{err: fmt.Errorf("%w: central returned an assignment without a task", errLocalExecution)}
	}
	switch {
	case task.GetCatalog() != nil:
		executor, supported := runtime.executor.(catalogExecutor)
		if !supported {
			return captureResult{err: fmt.Errorf("%w: probe executor does not support catalog capture", errLocalExecution)}
		}
		catalog, err := executor.CaptureCatalog(ctx, task)
		return captureResult{output: executionOutput{catalog: catalog}, err: err}
	case task.GetSeatMap() != nil:
		executor, supported := runtime.executor.(SeatMapExecutor)
		if !supported {
			return captureResult{err: fmt.Errorf("%w: probe executor does not support seat-map capture", errLocalExecution)}
		}
		liveSeat, err := executor.CaptureSeatMap(ctx, task)
		if err == nil {
			err = validateLiveSeatCapture(task, liveSeat)
		}
		return captureResult{output: executionOutput{liveSeat: liveSeat}, err: err}
	case task.GetSeatAvailability() != nil:
		executor, supported := runtime.executor.(SeatAvailabilityExecutor)
		if !supported {
			return captureResult{err: fmt.Errorf("%w: probe executor does not support seat-availability capture", errLocalExecution)}
		}
		liveSeat, err := executor.CaptureSeatAvailability(ctx, task)
		if err == nil {
			err = validateLiveSeatCapture(task, liveSeat)
		}
		return captureResult{output: executionOutput{liveSeat: liveSeat}, err: err}
	default:
		captures, err := runtime.executor.Capture(ctx, task)
		return captureResult{output: executionOutput{captures: captures}, err: err}
	}
}

func (runtime *Runtime) assignmentResult(
	output executionOutput,
	captureErr error,
	startedAt time.Time,
	finishedAt time.Time,
) (*observationpb.AssignmentResult, error) {
	runID, err := runtime.newRunID()
	if err != nil {
		return nil, err
	}
	result := &observationpb.AssignmentResult{}
	result.SetRunId(runID)
	result.SetStartedAt(timestamppb.New(startedAt))
	result.SetFinishedAt(timestamppb.New(finishedAt))
	if captureErr == nil {
		captureErr = validateExecutionOutput(output)
	}
	if captureErr != nil {
		runtime.logCaptureDiagnostic(captureErr, result.GetRunId())
		if deferred := captureDeferredReason(captureErr); deferred != nil {
			outcome := &observationpb.Deferred{}
			outcome.SetReason(deferred)
			result.SetDeferred(outcome)
			return result, nil // capture stopping points are represented in the generated result oneof
		}
		failed := &observationpb.Failed{}
		failed.SetReason(captureFailureReason(captureErr))
		result.SetFailed(failed)
		return result, nil //nolint:nilerr // capture failures are represented in the generated result oneof
	}
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
	result.SetCompleted(completed)
	return result, nil
}

var errInvalidExecutionOutput = errors.New("probe produced an invalid assignment result")

func validateExecutionOutput(output executionOutput) error {
	payloads := 0
	if output.liveSeat != nil {
		payloads++
	}
	if output.catalog != nil {
		payloads++
	}
	if output.captures != nil {
		payloads++
	}
	if payloads != 1 {
		return fmt.Errorf("%w: completed payload must contain exactly one variant", errInvalidExecutionOutput)
	}
	if output.liveSeat != nil {
		if err := protovalidate.Validate(output.liveSeat); err != nil {
			return fmt.Errorf("%w: %w", errInvalidExecutionOutput, err)
		}
	}
	if output.catalog != nil {
		if output.catalog.GetProvider() == nil || strings.TrimSpace(output.catalog.GetProvider().GetId()) == "" ||
			output.catalog.GetObservedAt() == nil {
			return fmt.Errorf("%w: catalog provider and observation time are required", errInvalidExecutionOutput)
		}
		if err := output.catalog.GetObservedAt().CheckValid(); err != nil {
			return fmt.Errorf("%w: invalid catalog observation time: %w", errInvalidExecutionOutput, err)
		}
	}
	if output.captures != nil && len(output.captures) == 0 {
		return fmt.Errorf("%w: schedule captures are empty", errInvalidExecutionOutput)
	}
	return nil
}

func validateLiveSeatCapture(
	task *observationpb.AssignmentTask,
	liveSeat *seatmappb.LiveSeatObservation,
) error {
	if liveSeat == nil {
		return fmt.Errorf("%w: live-seat observation is missing", errInvalidExecutionOutput)
	}
	if err := protovalidate.Validate(liveSeat); err != nil {
		return fmt.Errorf("%w: %w", errInvalidExecutionOutput, err)
	}
	auditoriumID, showtimeID, err := liveSeatAssignmentIDs(task)
	if err != nil {
		return err
	}
	if liveSeat.GetLayout().GetAuditoriumId() != auditoriumID || liveSeat.GetAvailability().GetAuditoriumId() != auditoriumID {
		return fmt.Errorf("%w: live-seat auditorium identity does not match assignment", errInvalidExecutionOutput)
	}
	if showtimeID != "" && liveSeat.GetAvailability().GetShowtimeId() != showtimeID {
		return fmt.Errorf("%w: live-seat showtime identity does not match assignment", errInvalidExecutionOutput)
	}
	return nil
}

func liveSeatAssignmentIDs(task *observationpb.AssignmentTask) (string, string, error) {
	switch {
	case task != nil && task.GetSeatMap() != nil:
		seatTask := task.GetSeatMap()
		if seatTask.GetAuditorium() == nil {
			return "", "", fmt.Errorf("%w: seat-map auditorium is missing", errInvalidExecutionOutput)
		}
		showtimeID := ""
		if seatTask.GetShowtime() != nil {
			showtimeID = seatTask.GetShowtime().GetId()
		}
		return seatTask.GetAuditorium().GetId(), showtimeID, nil
	case task != nil && task.GetSeatAvailability() != nil:
		seatTask := task.GetSeatAvailability()
		if seatTask.GetAuditorium() == nil || seatTask.GetShowtime() == nil {
			return "", "", fmt.Errorf("%w: seat-availability target is missing", errInvalidExecutionOutput)
		}
		return seatTask.GetAuditorium().GetId(), seatTask.GetShowtime().GetId(), nil
	default:
		return "", "", fmt.Errorf("%w: live-seat assignment target is missing", errInvalidExecutionOutput)
	}
}

/*
The generated contract intentionally carries typed reason messages rather
than transport-specific strings or retry flags.
*/
func captureDeferredReason(err error) *collectionpb.DeferredReason {
	switch {
	case errors.Is(err, cgv.ErrNoBookableShowtime):
		value := &collectionpb.DeferredReason{}
		value.SetNoBookableShowtime(&collectionpb.NoBookableShowtime{})
		return value
	case errors.Is(err, cgv.ErrTargetDateUnavailable):
		value := &collectionpb.DeferredReason{}
		value.SetTargetDateUnavailable(&collectionpb.TargetDateUnavailable{})
		return value
	default:
		return nil
	}
}

func captureFailureReason(err error) *collectionpb.FailureReason {
	reason := &collectionpb.FailureReason{}
	switch {
	case errors.Is(err, cgv.ErrIdentityMismatch):
		reason.SetIdentityMismatch(&collectionpb.IdentityMismatch{})
	case invalidCaptureResult(err):
		reason.SetInvalidResult(&collectionpb.InvalidResult{})
	case errors.Is(err, cgv.ErrProviderAccessBlocked):
		reason.SetProviderBlocked(&collectionpb.ProviderBlocked{})
	case errors.Is(err, cgv.ErrProviderThrottled):
		reason.SetProviderThrottled(&collectionpb.ProviderThrottled{})
	case errors.Is(err, cgv.ErrCaptchaRequired):
		reason.SetCaptchaRequired(&collectionpb.CaptchaRequired{})
	case errors.Is(err, cgv.ErrAuthenticationRequired):
		reason.SetAuthenticationRequired(&collectionpb.AuthenticationRequired{})
	case errors.Is(err, cgv.ErrUIContractChanged):
		reason.SetUiContractChanged(&collectionpb.UIContractChanged{})
	case errors.Is(err, cgv.ErrBrowserStartFailed):
		reason.SetBrowserStartFailed(&collectionpb.BrowserStartFailed{})
	case errors.Is(err, cgv.ErrProviderServerError):
		reason.SetProviderServerError(&collectionpb.ProviderServerError{})
	case errors.Is(err, context.DeadlineExceeded):
		reason.SetTimeout(&collectionpb.Timeout{})
	case errors.Is(err, cgv.ErrProviderTransport):
		reason.SetProviderTransportFailed(&collectionpb.ProviderTransportFailed{})
	default:
		reason.SetInvalidResult(&collectionpb.InvalidResult{})
	}
	return reason
}

func invalidCaptureResult(err error) bool {
	return errors.Is(err, errLocalExecution) || errors.Is(err, errInvalidExecutionOutput) ||
		errors.Is(err, cgv.ErrSeatAvailabilityIncomplete) || errors.Is(err, cgv.ErrProviderInvalidResult)
}

func (runtime *Runtime) logCaptureDiagnostic(err error, runID string) {
	runtime.config.Logger.Error("Observation capture returned a typed result",
		"domain", "observation", "event", "observation.assignment.capture.diagnostic", "outcome", "classified",
		"run_id", runID, "error_type", telemetry.ErrorType(err), telemetry.SafeDiagnosticKey, telemetry.SafeDiagnostic(err))
}

func (runtime *Runtime) commitResult(
	ctx context.Context,
	session Session,
	assignment *probepb.AssignmentLease,
	result *observationpb.AssignmentResult,
) error {
	backoff := runtime.config.ReconnectMinimum
	for {
		_, err := runtime.api.CommitResult(ctx, session, assignment, result)
		if err == nil || errors.Is(err, ErrLeaseExpired) {
			return err
		}
		if errors.Is(err, ErrUnauthorized) || !retryable(err) {
			return err
		}
		if !timestampAfter(assignment.GetLeaseExpiresAt(), runtime.clock().Add(backoff)) {
			return ErrLeaseExpired
		}
		runtime.logRetry("commit result", err, backoff)
		if err := runtime.wait(ctx, backoff); err != nil {
			return err
		}
		backoff = min(backoff*2, runtime.config.ReconnectMaximum)
	}
}

func (runtime *Runtime) heartbeatState() *probepb.HeartbeatRequest {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	active := make([]string, 0, 1)
	if runtime.activeID != "" {
		active = append(active, runtime.activeID)
	}
	available := 1
	if runtime.localDrain || runtime.remoteDrain || runtime.activeID != "" {
		available = 0
	}
	request := &probepb.HeartbeatRequest{}
	request.SetDraining(runtime.localDrain)
	request.SetActiveAssignmentIds(active)
	request.SetAvailableCapabilities(runtime.availableCapabilities())
	request.SetAvailableSlots(int32(available))
	health := &probepb.ProbeHealth{}
	health.SetHealthy(&probepb.Healthy{})
	request.SetHealth(health)
	return request
}

func (runtime *Runtime) availableCapabilities() []*observationpb.Capability {
	capabilities := runtime.config.Registration.GetCapabilities()
	if runtime.config.AvailableCapabilities != nil {
		capabilities = runtime.config.AvailableCapabilities()
	}
	registered := make(map[string]struct{}, len(runtime.config.Registration.GetCapabilities()))
	for _, capability := range runtime.config.Registration.GetCapabilities() {
		registered[capabilityKey(capability)] = struct{}{}
	}
	available := make([]*observationpb.Capability, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		key := capabilityKey(capability)
		if _, supported := registered[key]; !supported {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		available = append(available, capability)
	}
	return available
}

func (runtime *Runtime) setActive(assignmentID string) {
	runtime.mu.Lock()
	runtime.activeID = assignmentID
	runtime.mu.Unlock()
}

func (runtime *Runtime) draining() bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.localDrain || runtime.remoteDrain
}

func (runtime *Runtime) disconnect(session Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := runtime.heartbeatState()
	request.SetDraining(true)
	request.SetAvailableSlots(0)
	_, _ = runtime.api.HeartbeatProbe(ctx, session, request)
	_ = runtime.api.DisconnectProbe(ctx, session)
}

func (runtime *Runtime) newRunID() (string, error) {
	buffer := make([]byte, 18)
	if _, err := io.ReadFull(runtime.random, buffer); err != nil {
		return "", fmt.Errorf("generate Probe run id: %w", err)
	}
	return "run_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (runtime *Runtime) randomDuration(minimum, maximum time.Duration) (time.Duration, error) {
	if maximum == minimum {
		return minimum, nil
	}
	delta := maximum - minimum
	value, err := rand.Int(runtime.random, big.NewInt(int64(delta)+1))
	if err != nil {
		return 0, fmt.Errorf("choose Probe retry delay: %w", err)
	}
	return minimum + time.Duration(value.Int64()), nil
}

func (runtime *Runtime) logRetry(operation string, err error, delay time.Duration) {
	runtime.config.Logger.Warn("Central request will be retried",
		"domain", "probe", "event", "probe.central_request.retry_scheduled", "outcome", "requeued",
		"operation", operation, "retry_delay_ms", delay.Milliseconds(),
		"reason", "central_request_failed", "error_type", telemetry.ErrorType(err))
}

func resultOutcome(result *observationpb.AssignmentResult) string {
	if result == nil {
		return "failed"
	}
	if result.GetDeferred() != nil {
		return "deferred"
	}
	if result.GetFailed() != nil {
		return "failed"
	}
	completed := result.GetCompleted()
	if completed == nil {
		return "failed"
	}
	if completed.GetCatalog() != nil || completed.GetLiveSeat() != nil {
		return "succeeded"
	}
	schedule := completed.GetSchedule()
	if schedule == nil {
		return "failed"
	}
	captures := schedule.GetCaptures()
	if len(captures) == 0 {
		return "failed"
	}
	complete := 0
	for _, capture := range captures {
		if capture.GetComplete() {
			complete++
		}
	}
	if complete == len(captures) {
		return "succeeded"
	}
	if complete > 0 {
		return "degraded"
	}
	return "failed"
}

func assignmentTaskKind(task *observationpb.AssignmentTask) string {
	switch {
	case task != nil && task.GetCatalog() != nil:
		return "catalog"
	case task != nil && task.GetSeatMap() != nil:
		return "seat_map"
	case task != nil && task.GetSeatAvailability() != nil:
		return "seat_availability"
	case task != nil && task.GetSchedule() != nil:
		return "schedule"
	default:
		return "unknown"
	}
}

func capabilityKey(capability *observationpb.Capability) string {
	switch {
	case capability != nil && capability.GetScheduleCapture() != nil:
		return "cgv.schedule.capture"
	case capability != nil && capability.GetCatalogCapture() != nil:
		return "cgv.catalog.capture"
	case capability != nil && capability.GetSeatMapCapture() != nil:
		return "cgv.seat-map.capture"
	case capability != nil && capability.GetSeatAvailabilityCapture() != nil:
		return "cgv.seat-availability.capture"
	default:
		return ""
	}
}

func timestampTime(value *timestamppb.Timestamp) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.AsTime()
}

func timestampAfter(value *timestamppb.Timestamp, now time.Time) bool {
	return value != nil && value.AsTime().After(now)
}

func retryable(err error) bool {
	var apiError *APIError
	if errors.As(err, &apiError) {
		return apiError.Retryable || apiError.StatusCode >= 500
	}
	return !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrLeaseExpired) &&
		!errors.Is(err, ErrIncompatibleRuntime)
}

func defaultDuration(value *time.Duration, fallback time.Duration) {
	if *value == 0 {
		*value = fallback
	}
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
