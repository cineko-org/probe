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
	"sync"
	"time"

	central "github.com/cineko-org/contracts/v3"

	"golang.org/x/sync/errgroup"
)

const (
	DefaultClaimMinimum     = 2 * time.Second
	DefaultClaimMaximum     = 5 * time.Second
	DefaultReconnectMinimum = time.Second
	DefaultReconnectMaximum = 30 * time.Second
)

type Executor interface {
	Capture(context.Context, central.AssignmentTask) ([]central.Capture, error)
}

type catalogExecutor interface {
	CaptureCatalog(context.Context, central.AssignmentTask) (*central.CatalogSnapshot, error)
}

type SeatMapExecutor interface {
	CaptureSeatMap(context.Context, central.AssignmentTask) (*central.SeatMapVersion, error)
}

type executionOutput struct {
	captures []central.Capture
	catalog  *central.CatalogSnapshot
	seatMap  *central.SeatMapVersion
}

type captureResult struct {
	output executionOutput
	err    error
}

type Config struct {
	Registration          central.RegisterProbeRequest
	ClaimMinimum          time.Duration
	ClaimMaximum          time.Duration
	ReconnectMinimum      time.Duration
	ReconnectMaximum      time.Duration
	AvailableCapabilities func() []string
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
	if config.Registration.MaxConcurrency != 1 {
		return nil, errors.New("probe runtime currently requires maxConcurrency=1")
	}
	if config.Registration.Kind != "container" && config.Registration.Kind != "client" {
		return nil, errors.New("probe runtime kind must be container or client")
	}
	defaultDuration(&config.ClaimMinimum, DefaultClaimMinimum)
	defaultDuration(&config.ClaimMaximum, DefaultClaimMaximum)
	defaultDuration(&config.ReconnectMinimum, DefaultReconnectMinimum)
	defaultDuration(&config.ReconnectMaximum, DefaultReconnectMaximum)
	if config.ClaimMinimum <= 0 || config.ClaimMaximum < config.ClaimMinimum ||
		config.ReconnectMinimum <= 0 || config.ReconnectMaximum < config.ReconnectMinimum {
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
				runtime.config.Logger.Warn("Probe session expired before readiness; requesting a new bootstrap credential")
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
		runtime.config.Logger.Warn("Probe session expired; requesting a new bootstrap credential")
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
			if response.ProbeID == "" || response.AccessToken == "" || response.HeartbeatIntervalSeconds <= 0 {
				return Session{}, 0, errors.New("central returned an invalid Probe registration")
			}
			return Session{ProbeID: response.ProbeID, AccessToken: response.AccessToken},
				time.Duration(response.HeartbeatIntervalSeconds) * time.Second, nil
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
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := runtime.sendProbeHeartbeat(ctx, session); err != nil {
				if errors.Is(err, ErrUnauthorized) {
					return err
				}
				runtime.config.Logger.Warn("Probe heartbeat failed", "error", err)
			}
		}
	}
}

func (runtime *Runtime) sendProbeHeartbeat(ctx context.Context, session Session) error {
	request := runtime.heartbeatState()
	response, err := runtime.api.HeartbeatProbe(ctx, session, request)
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	runtime.remoteDrain = response.Drain
	runtime.mu.Unlock()
	return nil
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
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return err
			}
			if !retryable(err) {
				return err
			}
			runtime.config.Logger.Warn("Probe assignment claim failed", "error", err)
		} else if assignment != nil {
			if err := runtime.handleClaimedAssignment(ctx, session, *assignment); err != nil {
				return err
			}
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

func (runtime *Runtime) handleClaimedAssignment(
	ctx context.Context,
	session Session,
	assignment central.ClaimAssignmentResponse,
) error {
	runtime.setActive(assignment.AssignmentID)
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
		runtime.config.Logger.Error(
			"Probe assignment failed", "assignment_id", assignment.AssignmentID, "error", err,
		)
	}
	return nil
}

func (runtime *Runtime) rejectClaimWhileDraining(
	ctx context.Context,
	session Session,
	assignment central.ClaimAssignmentResponse,
) error {
	now := runtime.clock().UTC()
	result, err := runtime.assignmentResult(
		executionOutput{captures: []central.Capture{}}, errors.New("probe is draining"), now, now,
	)
	if err != nil {
		return err
	}
	return runtime.commitResult(ctx, session, assignment, result)
}

func (runtime *Runtime) executeAssignment(
	ctx context.Context,
	session Session,
	assignment central.ClaimAssignmentResponse,
) error {
	if !assignment.LeaseExpiresAt.After(runtime.clock()) || !assignment.Deadline.After(runtime.clock()) {
		return ErrLeaseExpired
	}
	startedAt := runtime.clock().UTC()
	assignmentContext, cancel := context.WithCancel(ctx)
	defer cancel()
	resultChannel := make(chan captureResult, 1)
	go func() { resultChannel <- runtime.captureAssignment(assignmentContext, assignment.Task) }()
	heartbeatInterval := max(
		assignment.LeaseExpiresAt.Sub(runtime.clock())/3,
		runtime.leaseHeartbeatMinimum,
	)
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case capture := <-resultChannel:
			finishedAt := runtime.clock().UTC()
			result, err := runtime.assignmentResult(capture.output, capture.err, startedAt, finishedAt)
			if err != nil {
				return err
			}
			return runtime.commitResult(ctx, session, assignment, result)
		case <-ticker.C:
			if !assignment.LeaseExpiresAt.After(runtime.clock()) {
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
				runtime.config.Logger.Warn(
					"Probe assignment heartbeat failed", "assignment_id", assignment.AssignmentID, "error", err,
				)
			case !response.LeaseExpiresAt.After(runtime.clock()):
				cancel()
				return errors.New("central returned an invalid assignment lease extension")
			default:
				assignment.LeaseExpiresAt = response.LeaseExpiresAt
			}
		}
	}
}

func (runtime *Runtime) captureAssignment(ctx context.Context, task central.AssignmentTask) captureResult {
	switch task.Kind {
	case central.CapabilityCGVCatalogCapture:
		executor, supported := runtime.executor.(catalogExecutor)
		if !supported {
			return captureResult{err: errors.New("probe executor does not support catalog capture")}
		}
		catalog, err := executor.CaptureCatalog(ctx, task)
		return captureResult{output: executionOutput{catalog: catalog}, err: err}
	case central.CapabilityCGVSeatMapCapture:
		executor, supported := runtime.executor.(SeatMapExecutor)
		if !supported {
			return captureResult{err: errors.New("probe executor does not support seat-map capture")}
		}
		seatMap, err := executor.CaptureSeatMap(ctx, task)
		return captureResult{output: executionOutput{seatMap: seatMap}, err: err}
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
) (central.AssignmentResult, error) {
	runID, err := runtime.newRunID()
	if err != nil {
		return central.AssignmentResult{}, err
	}
	status := resultStatus(output, captureErr)
	return central.AssignmentResult{
		RunID: runID, Status: status, StartedAt: startedAt, FinishedAt: finishedAt,
		Captures: output.captures, Catalog: output.catalog, SeatMap: output.seatMap,
	}, nil
}

func (runtime *Runtime) commitResult(
	ctx context.Context,
	session Session,
	assignment central.ClaimAssignmentResponse,
	result central.AssignmentResult,
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
		if !assignment.LeaseExpiresAt.After(runtime.clock().Add(backoff)) {
			return ErrLeaseExpired
		}
		runtime.logRetry("commit result", err, backoff)
		if err := runtime.wait(ctx, backoff); err != nil {
			return err
		}
		backoff = min(backoff*2, runtime.config.ReconnectMaximum)
	}
}

func (runtime *Runtime) heartbeatState() central.ProbeHeartbeatRequest {
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
	return central.ProbeHeartbeatRequest{
		Draining:            runtime.localDrain,
		ActiveAssignmentIDs: active, AvailableCapabilities: runtime.availableCapabilities(),
		AvailableSlots: available, Health: "healthy",
	}
}

func (runtime *Runtime) availableCapabilities() []string {
	capabilities := runtime.config.Registration.Capabilities
	if runtime.config.AvailableCapabilities != nil {
		capabilities = runtime.config.AvailableCapabilities()
	}
	registered := make(map[string]struct{}, len(runtime.config.Registration.Capabilities))
	for _, capability := range runtime.config.Registration.Capabilities {
		registered[capability] = struct{}{}
	}
	available := make([]string, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if _, supported := registered[capability]; !supported {
			continue
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
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
	request.Draining = true
	request.AvailableSlots = 0
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
	runtime.config.Logger.Warn("Central request will be retried", "operation", operation, "delay", delay, "error", err)
}

func resultStatus(output executionOutput, captureErr error) string {
	if captureErr != nil {
		return "failed"
	}
	if output.catalog != nil || output.seatMap != nil {
		return "completed"
	}
	if len(output.captures) == 0 {
		return "failed"
	}
	complete := 0
	for _, capture := range output.captures {
		if capture.Complete {
			complete++
		}
	}
	if complete == len(output.captures) {
		return "completed"
	}
	if complete > 0 {
		return "partial"
	}
	return "failed"
}

func retryable(err error) bool {
	var apiError *APIError
	if errors.As(err, &apiError) {
		return apiError.Retryable || apiError.StatusCode >= 500
	}
	return !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrLeaseExpired)
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
