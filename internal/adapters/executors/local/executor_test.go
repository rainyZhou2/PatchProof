package local

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls int
	specs []domain.StepSpec
	run   func(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error)
}

func (f *fakeRunner) RunStep(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error) {
	f.mu.Lock()
	f.calls++
	f.specs = append(f.specs, spec)
	run := f.run
	f.mu.Unlock()
	return run(ctx, spec)
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRunner) lastSpec() domain.StepSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) == 0 {
		return domain.StepSpec{}
	}
	return f.specs[len(f.specs)-1]
}

func newSpec(deadline time.Time) domain.StepSpec {
	return domain.StepSpec{
		RunID:        domain.NewRunID(),
		StepID:       domain.NewStepID(),
		AttemptID:    domain.NewAttemptID(),
		TenantID:     domain.NewTenantID(),
		Generation:   1,
		Kind:         domain.StepKindCompile,
		Language:     "go",
		Engine:       "go-analyzer",
		Deadline:     deadline,
		AttemptToken: "attempt-token-value",
		CallbackURL:  "https://controller.test/v1/callback",
		Requirements: domain.ResourceRequirements{
			Architecture:    "amd64",
			Isolation:       domain.IsolationTrustedLocal,
			CPUMilli:        500,
			MemoryMiB:       256,
			Network:         domain.NetworkNone,
			DeadlineSeconds: 60,
		},
	}
}

func successResult() domain.StepResult {
	return domain.StepResult{
		Outcome:      domain.OutcomeSucceeded,
		ResultDigest: "sha256:" + strings.Repeat("a", 64),
		FinishedAt:   time.Now().UTC(),
	}
}

func waitForState(t *testing.T, ex *Executor, ref ports.ExecutionRef, want ports.ExecutionState) ports.ExecutionObservation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		obs, err := ex.Observe(context.Background(), ref)
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if obs.State == want {
			return obs
		}
		time.Sleep(5 * time.Millisecond)
	}
	obs, _ := ex.Observe(context.Background(), ref)
	t.Fatalf("state = %s, want %s (observation: %+v)", obs.State, want, obs)
	return ports.ExecutionObservation{}
}

func TestStartSuccess(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeRunner{run: func(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error) {
		return successResult(), nil
	}}
	ex, err := New(Config{Name: "local-a", MaxConcurrent: 2, WorkDir: workDir}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()

	spec := newSpec(time.Now().Add(time.Minute))
	ref, err := ex.Start(context.Background(), ports.ExecutionRequest{Spec: spec})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ref.Executor != "local-a" || ref.ExternalID != string(spec.AttemptID) {
		t.Fatalf("unexpected ref: %+v", ref)
	}

	obs := waitForState(t, ex, ref, ports.ExecutionCompleted)
	if obs.Result == nil {
		t.Fatal("completed observation has no result")
	}
	if obs.Result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("result outcome = %s", obs.Result.Outcome)
	}

	got := runner.lastSpec()
	if got.AttemptToken != spec.AttemptToken || got.CallbackURL != spec.CallbackURL {
		t.Fatalf("runner spec secrets were altered: %+v", got)
	}

	deadline := time.Now().Add(time.Second)
	for {
		entries, readErr := os.ReadDir(workDir)
		if readErr == nil && len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempt workdir was not cleaned up: %v (entries=%d)", readErr, len(entries))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartDeadlineExceeded(t *testing.T) {
	runner := &fakeRunner{run: func(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error) {
		<-ctx.Done()
		return domain.StepResult{}, ctx.Err()
	}}
	ex, err := New(Config{MaxConcurrent: 1, WorkDir: t.TempDir()}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()

	ref, err := ex.Start(context.Background(), ports.ExecutionRequest{Spec: newSpec(time.Now().Add(50 * time.Millisecond))})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	obs := waitForState(t, ex, ref, ports.ExecutionFailed)
	if obs.ErrorClass != domain.ErrTimeout {
		t.Fatalf("error class = %s, want %s", obs.ErrorClass, domain.ErrTimeout)
	}
}

func TestCancel(t *testing.T) {
	runner := &fakeRunner{run: func(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error) {
		<-ctx.Done()
		return domain.StepResult{}, ctx.Err()
	}}
	ex, err := New(Config{MaxConcurrent: 1, WorkDir: t.TempDir()}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()

	ref, err := ex.Start(context.Background(), ports.ExecutionRequest{Spec: newSpec(time.Now().Add(time.Minute))})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := ex.Cancel(context.Background(), ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	obs, err := ex.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.State != ports.ExecutionCancelled || obs.ErrorClass != domain.ErrCancelled {
		t.Fatalf("observation = %+v, want cancelled", obs)
	}
	if err := ex.Cancel(context.Background(), ports.ExecutionRef{ExternalID: "unknown"}); err != nil {
		t.Fatalf("Cancel unknown: %v", err)
	}
}

func TestStartIdempotent(t *testing.T) {
	runner := &fakeRunner{run: func(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error) {
		return successResult(), nil
	}}
	ex, err := New(Config{MaxConcurrent: 2, WorkDir: t.TempDir()}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()

	spec := newSpec(time.Now().Add(time.Minute))
	request := ports.ExecutionRequest{Spec: spec}
	first, err := ex.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	second, err := ex.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if first != second {
		t.Fatalf("refs differ: %+v vs %+v", first, second)
	}
	waitForState(t, ex, first, ports.ExecutionCompleted)
	if got := runner.callCount(); got != 1 {
		t.Fatalf("runner called %d times, want 1", got)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	var current, peak atomic.Int32
	runner := &fakeRunner{run: func(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error) {
		running := current.Add(1)
		for {
			observed := peak.Load()
			if running <= observed || peak.CompareAndSwap(observed, running) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		current.Add(-1)
		return successResult(), nil
	}}
	ex, err := New(Config{MaxConcurrent: 2, WorkDir: t.TempDir()}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const total = 4
	refs := make(chan ports.ExecutionRef, total)
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		spec := newSpec(time.Now().Add(time.Minute))
		go func() {
			ref, startErr := ex.Start(ctx, ports.ExecutionRequest{Spec: spec})
			refs <- ref
			errs <- startErr
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("expected two steps to start")
		}
	}
	select {
	case <-entered:
		t.Fatal("third step started above the concurrency limit")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	for i := 0; i < total; i++ {
		if startErr := <-errs; startErr != nil {
			t.Fatalf("Start: %v", startErr)
		}
	}
	if got := peak.Load(); got > 2 {
		t.Fatalf("peak concurrency = %d, want at most 2", got)
	}
	for i := 0; i < total; i++ {
		waitForState(t, ex, <-refs, ports.ExecutionCompleted)
	}
}

func TestIsolationRejected(t *testing.T) {
	runner := &fakeRunner{run: func(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error) {
		return successResult(), nil
	}}
	ex, err := New(Config{MaxConcurrent: 1, WorkDir: t.TempDir(), AllowedIsolation: domain.IsolationTrustedLocal}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()

	spec := newSpec(time.Now().Add(time.Minute))
	spec.Requirements.Isolation = domain.IsolationVM
	_, err = ex.Start(context.Background(), ports.ExecutionRequest{Spec: spec})
	if err == nil {
		t.Fatal("expected isolation rejection")
	}
	var classified *Error
	if !errors.As(err, &classified) || classified.Class != domain.ErrSandboxUnavailable {
		t.Fatalf("error = %v, want sandbox unavailable", err)
	}
	if runner.callCount() != 0 {
		t.Fatal("runner must not be invoked for rejected isolation")
	}
}

func TestCapabilities(t *testing.T) {
	ex, err := New(Config{Name: "local-b", Region: "region-1", MaxConcurrent: 3, WorkDir: t.TempDir()}, &fakeRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()
	caps, err := ex.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Name != "local-b" || caps.Region != "region-1" || caps.MaxConcurrent != 3 {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
	if len(caps.IsolationTiers) != 1 || caps.IsolationTiers[0] != domain.IsolationTrustedLocal {
		t.Fatalf("isolation tiers = %v", caps.IsolationTiers)
	}
	if len(caps.Features) != 1 || caps.Features[0] != featureInProcess {
		t.Fatalf("features = %v", caps.Features)
	}
	if len(caps.Languages) == 0 || len(caps.Engines) == 0 {
		t.Fatalf("languages/engines must not be empty: %+v", caps)
	}
}

func TestObserveUnknownIsLost(t *testing.T) {
	ex, err := New(Config{MaxConcurrent: 1, WorkDir: t.TempDir()}, &fakeRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()
	obs, err := ex.Observe(context.Background(), ports.ExecutionRef{ExternalID: "missing"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.State != ports.ExecutionLost {
		t.Fatalf("state = %s, want LOST", obs.State)
	}
}
