// Package local provides an in-process executor. Steps run in goroutines
// inside the host process and are only appropriate for workloads that have
// already been admitted for the trusted-local isolation tier.
package local

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/clock"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	defaultName         = "local"
	featureInProcess    = "in-process"
	maxMessageLength    = 512
	terminalRetention   = 24 * time.Hour
	closeWaitTimeout    = 5 * time.Second
	unknownWorkdirToken = "attempt"
)

var (
	supportedLanguages = []string{"go"}
	supportedEngines   = []string{"go-analyzer", "go-differential", "go-fuzz", "z3-bounded", "semgrep"}
	supportedFeatures  = []string{featureInProcess}
)

// Config configures the local executor.
type Config struct {
	// Name is the executor identity reported to the scheduler.
	Name string
	// Region is the region reported in capabilities.
	Region string
	// MaxConcurrent bounds the number of steps executing at once.
	MaxConcurrent int
	// WorkDir is the parent directory for per-attempt scratch directories.
	WorkDir string
	// AllowedIsolation is the highest isolation tier the host may serve.
	AllowedIsolation domain.IsolationTier
	// ActiveDeadline is used when a step spec does not carry its own deadline.
	ActiveDeadline time.Duration
	// Clock supplies the current time and defaults to the system clock.
	Clock ports.Clock
}

// Error reports a classified start failure.
type Error struct {
	Class   domain.ErrorClass
	Message string
}

func (e *Error) Error() string { return e.Message }

// ErrorClass returns the domain classification of the failure.
func (e *Error) ErrorClass() domain.ErrorClass { return e.Class }

// Executor runs step specs through an in-process ports.StepRunner.
var _ ports.Executor = (*Executor)(nil)

type Executor struct {
	cfg    Config
	runner ports.StepRunner

	sem       chan struct{}
	baseCtx   context.Context
	cancelAll context.CancelFunc

	mu         sync.Mutex
	executions map[domain.AttemptID]*execution
	closed     bool

	wg sync.WaitGroup
}

type execution struct {
	ref     ports.ExecutionRef
	cancel  context.CancelFunc
	workDir string

	mu         sync.Mutex
	state      ports.ExecutionState
	class      domain.ErrorClass
	message    string
	result     *domain.StepResult
	finishedAt time.Time
}

// New validates cfg and returns a local executor bound to runner.
func New(cfg Config, runner ports.StepRunner) (*Executor, error) {
	if runner == nil {
		return nil, errors.New("local executor: step runner is required")
	}
	if cfg.MaxConcurrent <= 0 {
		return nil, fmt.Errorf("local executor: max concurrent must be positive, got %d", cfg.MaxConcurrent)
	}
	if cfg.ActiveDeadline < 0 {
		return nil, fmt.Errorf("local executor: active deadline must not be negative")
	}
	if cfg.Name == "" {
		cfg.Name = defaultName
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.AllowedIsolation == "" {
		cfg.AllowedIsolation = domain.IsolationTrustedLocal
	}
	if !cfg.AllowedIsolation.Valid() {
		return nil, fmt.Errorf("local executor: unknown isolation tier %q", cfg.AllowedIsolation)
	}

	baseCtx, cancel := context.WithCancel(context.Background())
	return &Executor{
		cfg:        cfg,
		runner:     runner,
		sem:        make(chan struct{}, cfg.MaxConcurrent),
		baseCtx:    baseCtx,
		cancelAll:  cancel,
		executions: make(map[domain.AttemptID]*execution),
	}, nil
}

// Capabilities reports the static capabilities of the local backend.
func (e *Executor) Capabilities(ctx context.Context) (ports.ExecutorCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return ports.ExecutorCapabilities{}, err
	}
	return ports.ExecutorCapabilities{
		Name:           e.cfg.Name,
		ExecutorKind:   "local",
		Region:         e.cfg.Region,
		Architectures:  []string{runtime.GOARCH},
		Languages:      append([]string(nil), supportedLanguages...),
		Engines:        append([]string(nil), supportedEngines...),
		IsolationTiers: []domain.IsolationTier{domain.IsolationTrustedLocal},
		RuntimeClasses: nil,
		Features:       append([]string(nil), supportedFeatures...),
		MaxConcurrent:  e.cfg.MaxConcurrent,
	}, nil
}

// Start launches a step. It is idempotent per attempt ID and blocks while the
// executor is at capacity or the caller context is cancelled.
func (e *Executor) Start(ctx context.Context, request ports.ExecutionRequest) (ports.ExecutionRef, error) {
	spec := request.Spec
	if err := validateSpec(spec); err != nil {
		return ports.ExecutionRef{}, &Error{Class: domain.ErrValidationFailed, Message: "invalid step spec: " + err.Error()}
	}
	requirements := spec.Requirements
	if requirements.Isolation.Rank() > e.cfg.AllowedIsolation.Rank() {
		return ports.ExecutionRef{}, &Error{
			Class:   domain.ErrSandboxUnavailable,
			Message: fmt.Sprintf("local executor allows isolation up to %q, step requires %q", e.cfg.AllowedIsolation, requirements.Isolation),
		}
	}
	if requirements.GPUCount > 0 {
		return ports.ExecutionRef{}, &Error{Class: domain.ErrSandboxUnavailable, Message: "local executor cannot provision GPUs"}
	}
	if missing := missingFeatures(requirements.RequiredFeatures); len(missing) > 0 {
		return ports.ExecutionRef{}, &Error{
			Class:   domain.ErrSandboxUnavailable,
			Message: fmt.Sprintf("local executor lacks required features: %s", strings.Join(missing, ", ")),
		}
	}

	if existing, ok := e.lookup(spec.AttemptID); ok {
		return existing, nil
	}

	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return ports.ExecutionRef{}, &Error{Class: domain.ErrCancelled, Message: "start cancelled while waiting for capacity: " + ctx.Err().Error()}
	case <-e.baseCtx.Done():
		return ports.ExecutionRef{}, &Error{Class: domain.ErrCancelled, Message: "local executor is closed"}
	}

	e.mu.Lock()
	if existing, ok := e.executions[spec.AttemptID]; ok {
		ref := existing.ref
		e.mu.Unlock()
		<-e.sem
		return ref, nil
	}
	if e.closed {
		e.mu.Unlock()
		<-e.sem
		return ports.ExecutionRef{}, &Error{Class: domain.ErrCancelled, Message: "local executor is closed"}
	}
	deadline, err := e.deadline(spec)
	if err != nil {
		e.mu.Unlock()
		<-e.sem
		return ports.ExecutionRef{}, err
	}
	workDir, err := e.newWorkDir(spec.AttemptID)
	if err != nil {
		e.mu.Unlock()
		<-e.sem
		return ports.ExecutionRef{}, err
	}

	runCtx, cancel := context.WithDeadline(e.baseCtx, deadline)
	ex := &execution{
		ref: ports.ExecutionRef{
			Executor:   e.cfg.Name,
			ExternalID: string(spec.AttemptID),
		},
		cancel:  cancel,
		workDir: workDir,
		state:   ports.ExecutionPending,
	}
	e.executions[spec.AttemptID] = ex
	e.pruneLocked(e.cfg.Clock.Now())
	ref := ex.ref
	e.wg.Add(1)
	e.mu.Unlock()

	go e.run(runCtx, ex, spec)
	return ref, nil
}

// Observe returns the current execution state without blocking.
func (e *Executor) Observe(ctx context.Context, ref ports.ExecutionRef) (ports.ExecutionObservation, error) {
	if err := ctx.Err(); err != nil {
		return ports.ExecutionObservation{}, err
	}
	e.mu.Lock()
	ex := e.executions[domain.AttemptID(ref.ExternalID)]
	e.mu.Unlock()
	if ex == nil {
		return ports.ExecutionObservation{
			State:      ports.ExecutionLost,
			ErrorClass: domain.ErrWorkerLost,
			Message:    "execution " + truncate(ref.ExternalID, 128) + " is unknown to this executor",
		}, nil
	}
	return ex.observation(), nil
}

// Cancel requests cancellation of a running step. Unknown executions are
// treated as already cancelled.
func (e *Executor) Cancel(ctx context.Context, ref ports.ExecutionRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	ex := e.executions[domain.AttemptID(ref.ExternalID)]
	e.mu.Unlock()
	if ex == nil {
		return nil
	}
	ex.cancelExecution(e.cfg.Clock.Now())
	return nil
}

// Close cancels every running step and waits briefly for them to stop.
func (e *Executor) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	executions := make([]*execution, 0, len(e.executions))
	for _, ex := range e.executions {
		executions = append(executions, ex)
	}
	e.mu.Unlock()

	e.cancelAll()
	now := e.cfg.Clock.Now()
	for _, ex := range executions {
		ex.cancelExecution(now)
	}

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(closeWaitTimeout):
		return errors.New("local executor: close timed out waiting for running steps")
	}
}

func (e *Executor) run(ctx context.Context, ex *execution, spec domain.StepSpec) {
	defer e.wg.Done()
	defer func() { e.removeWorkDir(ex) }()
	defer func() { <-e.sem }()
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("local executor: step runner panicked", slog.String("attempt_id", string(spec.AttemptID)), slog.Any("panic", recovered))
			ex.finish(ports.ExecutionFailed, domain.ErrExecutorError, fmt.Sprintf("step runner panicked: %v", recovered), nil, e.cfg.Clock.Now())
		}
	}()

	ex.markRunning()
	result, err := e.runner.RunStep(ctx, spec)
	if err != nil {
		ex.finish(ports.ExecutionFailed, classifyFailure(ctx, err), err.Error(), nil, e.cfg.Clock.Now())
		return
	}
	if err := result.Validate(); err != nil {
		ex.finish(ports.ExecutionFailed, domain.ErrExecutorError, "invalid step result: "+err.Error(), nil, e.cfg.Clock.Now())
		return
	}
	ex.finish(ports.ExecutionCompleted, "", "", &result, e.cfg.Clock.Now())
}

func (e *Executor) removeWorkDir(ex *execution) {
	if ex.workDir == "" {
		return
	}
	if err := os.RemoveAll(ex.workDir); err != nil {
		slog.Warn("local executor: could not remove attempt workdir", slog.String("workdir", ex.workDir), slog.String("error", err.Error()))
	}
}

func (e *Executor) newWorkDir(attemptID domain.AttemptID) (string, error) {
	if err := os.MkdirAll(e.cfg.WorkDir, 0o700); err != nil {
		return "", &Error{Class: domain.ErrExecutorError, Message: "create work root: " + err.Error()}
	}
	dir, err := os.MkdirTemp(e.cfg.WorkDir, "pp-"+workdirToken(string(attemptID))+"-")
	if err != nil {
		return "", &Error{Class: domain.ErrExecutorError, Message: "create attempt workdir: " + err.Error()}
	}
	return dir, nil
}

func (e *Executor) deadline(spec domain.StepSpec) (time.Time, error) {
	now := e.cfg.Clock.Now()
	deadline := spec.Deadline
	if deadline.IsZero() {
		if e.cfg.ActiveDeadline <= 0 {
			return time.Time{}, &Error{Class: domain.ErrValidationFailed, Message: "step spec has no deadline and no active deadline is configured"}
		}
		deadline = now.Add(e.cfg.ActiveDeadline)
	}
	if !deadline.After(now) {
		return time.Time{}, &Error{Class: domain.ErrTimeout, Message: "step deadline has already passed"}
	}
	return deadline, nil
}

func (e *Executor) lookup(attemptID domain.AttemptID) (ports.ExecutionRef, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ex, ok := e.executions[attemptID]
	if !ok {
		return ports.ExecutionRef{}, false
	}
	return ex.ref, true
}

func (e *Executor) pruneLocked(now time.Time) {
	for id, ex := range e.executions {
		if ex.terminalSince(now) {
			delete(e.executions, id)
		}
	}
}

func (x *execution) markRunning() {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.state == ports.ExecutionPending {
		x.state = ports.ExecutionRunning
	}
}

func (x *execution) finish(state ports.ExecutionState, class domain.ErrorClass, message string, result *domain.StepResult, at time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.state != ports.ExecutionPending && x.state != ports.ExecutionRunning {
		return
	}
	x.state = state
	x.class = class
	x.message = truncate(message, maxMessageLength)
	x.result = result
	x.finishedAt = at
}

func (x *execution) cancelExecution(at time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.state == ports.ExecutionPending || x.state == ports.ExecutionRunning {
		x.state = ports.ExecutionCancelled
		x.class = domain.ErrCancelled
		x.message = "execution cancelled"
		x.result = nil
		x.finishedAt = at
	}
	x.cancel()
}

func (x *execution) terminalSince(now time.Time) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch x.state {
	case ports.ExecutionCompleted, ports.ExecutionFailed, ports.ExecutionCancelled, ports.ExecutionLost:
		return !x.finishedAt.IsZero() && now.Sub(x.finishedAt) > terminalRetention
	default:
		return false
	}
}

func (x *execution) observation() ports.ExecutionObservation {
	x.mu.Lock()
	defer x.mu.Unlock()
	obs := ports.ExecutionObservation{
		State:      x.state,
		ErrorClass: x.class,
		Message:    x.message,
	}
	if x.result != nil {
		result := *x.result
		obs.Result = &result
	}
	return obs
}

func validateSpec(spec domain.StepSpec) error {
	if err := spec.RunID.Validate(); err != nil {
		return err
	}
	if err := spec.StepID.Validate(); err != nil {
		return err
	}
	if err := spec.AttemptID.Validate(); err != nil {
		return err
	}
	if err := spec.TenantID.Validate(); err != nil {
		return err
	}
	if spec.Generation < 0 {
		return domain.Validationf("generation", "must not be negative")
	}
	return spec.Requirements.Validate()
}

func classifyFailure(ctx context.Context, err error) domain.ErrorClass {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return domain.ErrTimeout
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return domain.ErrCancelled
	default:
		return domain.ErrExecutorError
	}
}

func missingFeatures(required []string) []string {
	var missing []string
	for _, feature := range required {
		found := false
		for _, supported := range supportedFeatures {
			if strings.EqualFold(feature, supported) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, feature)
		}
	}
	return missing
}

func workdirToken(attemptID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(attemptID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			if b.Len() >= 8 {
				break
			}
		}
	}
	if b.Len() == 0 {
		return unknownWorkdirToken
	}
	return b.String()
}

func truncate(value string, limit int) string {
	if value == "" || len(value) <= limit {
		return value
	}
	return value[:limit]
}
