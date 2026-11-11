package clusteragent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type fakeExecutor struct {
	mu        sync.Mutex
	caps      ports.ExecutorCapabilities
	capsErr   error
	startFn   func(context.Context, ports.ExecutionRequest) (ports.ExecutionRef, error)
	observeFn func(context.Context, ports.ExecutionRef) (ports.ExecutionObservation, error)
	cancelFn  func(context.Context, ports.ExecutionRef) error
	starts    []ports.ExecutionRequest
	observes  []ports.ExecutionRef
	cancels   []ports.ExecutionRef
}

func (f *fakeExecutor) Capabilities(ctx context.Context) (ports.ExecutorCapabilities, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.capsErr != nil {
		return ports.ExecutorCapabilities{}, f.capsErr
	}
	return f.caps, nil
}

func (f *fakeExecutor) Start(ctx context.Context, request ports.ExecutionRequest) (ports.ExecutionRef, error) {
	f.mu.Lock()
	f.starts = append(f.starts, request)
	fn := f.startFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, request)
	}
	return ports.ExecutionRef{Executor: "kubernetes", Namespace: "pp-exec", ExternalID: "pp-abcdef12-12345678"}, nil
}

func (f *fakeExecutor) Observe(ctx context.Context, ref ports.ExecutionRef) (ports.ExecutionObservation, error) {
	f.mu.Lock()
	f.observes = append(f.observes, ref)
	fn := f.observeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, ref)
	}
	return ports.ExecutionObservation{State: ports.ExecutionRunning}, nil
}

func (f *fakeExecutor) Cancel(ctx context.Context, ref ports.ExecutionRef) error {
	f.mu.Lock()
	f.cancels = append(f.cancels, ref)
	fn := f.cancelFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, ref)
	}
	return nil
}

func (f *fakeExecutor) setCapabilitiesError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capsErr = err
}

func (f *fakeExecutor) lastStart(t *testing.T) ports.ExecutionRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) == 0 {
		t.Fatal("executor Start was not called")
	}
	return f.starts[len(f.starts)-1]
}

func (f *fakeExecutor) lastObserve(t *testing.T) ports.ExecutionRef {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.observes) == 0 {
		t.Fatal("executor Observe was not called")
	}
	return f.observes[len(f.observes)-1]
}

func (f *fakeExecutor) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancels)
}

type fakeRegistrar struct {
	mu       sync.Mutex
	clusters []domain.Cluster
	err      error
	notified chan struct{}
}

func (f *fakeRegistrar) UpsertCluster(ctx context.Context, cluster domain.Cluster) error {
	f.mu.Lock()
	f.clusters = append(f.clusters, cluster)
	err := f.err
	f.mu.Unlock()
	if f.notified != nil {
		select {
		case f.notified <- struct{}{}:
		default:
		}
	}
	return err
}

func (f *fakeRegistrar) last(t *testing.T) domain.Cluster {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.clusters) == 0 {
		t.Fatal("registrar was not called")
	}
	return f.clusters[len(f.clusters)-1]
}

func (f *fakeRegistrar) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clusters)
}

type classifiedError struct {
	class   domain.ErrorClass
	message string
}

func (e *classifiedError) Error() string { return e.message }

func (e *classifiedError) ErrorClass() domain.ErrorClass { return e.class }

func newTestAgent(t *testing.T, mutate func(*Config)) (*Server, *fakeExecutor, *fakeRegistrar) {
	t.Helper()
	executor := &fakeExecutor{caps: ports.ExecutorCapabilities{
		Name:           "fake-executor",
		ExecutorKind:   "kubernetes",
		Region:         "region-1",
		Architectures:  []string{"amd64"},
		Languages:      []string{"go"},
		Engines:        []string{"go-analyzer"},
		IsolationTiers: []domain.IsolationTier{domain.IsolationContainer},
		RuntimeClasses: []string{"gvisor"},
		Features:       []string{"callback"},
		MaxConcurrent:  5,
	}}
	registrar := &fakeRegistrar{notified: make(chan struct{}, 32)}
	cfg := Config{
		ListenAddr:        "127.0.0.1:0",
		Token:             "agent-token",
		Executor:          executor,
		Registrar:         registrar,
		HeartbeatInterval: time.Hour,
		ClusterInfo:       domain.Cluster{ID: "cluster-a", Region: "region-1"},
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server, executor, registrar
}

func validSpec() domain.StepSpec {
	return domain.StepSpec{
		RunID:      domain.NewRunID(),
		StepID:     domain.NewStepID(),
		AttemptID:  domain.NewAttemptID(),
		TenantID:   domain.NewTenantID(),
		Generation: 1,
		Kind:       domain.StepKindCompile,
		Language:   "go",
		Engine:     "go-analyzer",
		Deadline:   time.Now().Add(time.Minute),
		Requirements: domain.ResourceRequirements{
			Architecture:    "amd64",
			Isolation:       domain.IsolationContainer,
			CPUMilli:        250,
			MemoryMiB:       128,
			Network:         domain.NetworkNone,
			DeadlineSeconds: 60,
		},
	}
}

func serve(server *Server, method, target, token string, body io.Reader) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, body)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) problem {
	t.Helper()
	if got := recorder.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("content type = %q, want application/problem+json", got)
	}
	var body problem
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	return body
}

func TestAuthRejection(t *testing.T) {
	server, _, _ := newTestAgent(t, nil)

	for _, test := range []struct {
		name   string
		header string
	}{
		{name: "missing"},
		{name: "wrong token", header: "Bearer wrong-token"},
		{name: "wrong scheme", header: "Token agent-token"},
		{name: "empty bearer", header: "Bearer "},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			if recorder.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing WWW-Authenticate header")
			}
			body := decodeProblem(t, recorder)
			if body.Code != "unauthorized" || body.Status != http.StatusUnauthorized || body.Title == "" || body.Type == "" {
				t.Fatalf("problem = %+v", body)
			}
		})
	}
}

func TestHealthNeedsNoAuth(t *testing.T) {
	server, _, _ := newTestAgent(t, nil)
	recorder := serve(server, http.MethodGet, "/healthz", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"ok"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestCapabilitiesDelegation(t *testing.T) {
	server, executor, _ := newTestAgent(t, nil)
	recorder := serve(server, http.MethodGet, "/v1/capabilities", "agent-token", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var capabilities ports.ExecutorCapabilities
	if err := json.Unmarshal(recorder.Body.Bytes(), &capabilities); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if capabilities.Name != executor.caps.Name || capabilities.MaxConcurrent != 5 {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestReadinessReflectsExecutor(t *testing.T) {
	server, executor, _ := newTestAgent(t, nil)
	recorder := serve(server, http.MethodGet, "/readyz", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	executor.setCapabilitiesError(&classifiedError{class: domain.ErrExecutorError, message: "not ready"})
	recorder = serve(server, http.MethodGet, "/readyz", "", nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	body := decodeProblem(t, recorder)
	if body.Code != "not_ready" {
		t.Fatalf("code = %q", body.Code)
	}
}

func TestStartDelegation(t *testing.T) {
	server, executor, _ := newTestAgent(t, nil)
	spec := validSpec()
	payload, err := json.Marshal(map[string]any{
		"spec":          spec,
		"attempt_token": "attempt-token-1",
		"callback_url":  "https://controller.test/callback",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	recorder := serve(server, http.MethodPost, "/v1/executions", "agent-token", bytes.NewReader(payload))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var ref ports.ExecutionRef
	if err := json.Unmarshal(recorder.Body.Bytes(), &ref); err != nil {
		t.Fatalf("decode ref: %v", err)
	}
	if ref.ExternalID != "pp-abcdef12-12345678" {
		t.Fatalf("ref = %+v", ref)
	}
	started := executor.lastStart(t)
	if started.Spec.AttemptID != spec.AttemptID {
		t.Fatalf("attempt id = %s", started.Spec.AttemptID)
	}
	if started.AttemptToken != "attempt-token-1" || started.CallbackURL != "https://controller.test/callback" {
		t.Fatalf("credentials not forwarded: %+v", started)
	}
}

func TestStartValidation(t *testing.T) {
	server, executor, _ := newTestAgent(t, nil)
	spec := validSpec()
	payload, err := json.Marshal(map[string]any{"spec": spec, "attempt_token": ""})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	recorder := serve(server, http.MethodPost, "/v1/executions", "agent-token", bytes.NewReader(payload))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := decodeProblem(t, recorder); body.Code != "invalid_request" {
		t.Fatalf("code = %q", body.Code)
	}
	if len(executor.starts) != 0 {
		t.Fatal("executor must not be called for an invalid request")
	}
}

func TestStartClassifiedFailure(t *testing.T) {
	server, executor, _ := newTestAgent(t, nil)
	executor.startFn = func(context.Context, ports.ExecutionRequest) (ports.ExecutionRef, error) {
		return ports.ExecutionRef{}, &classifiedError{class: domain.ErrSandboxUnavailable, message: "no sandbox"}
	}
	spec := validSpec()
	payload, err := json.Marshal(map[string]any{"spec": spec, "attempt_token": "token"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	recorder := serve(server, http.MethodPost, "/v1/executions", "agent-token", bytes.NewReader(payload))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := decodeProblem(t, recorder); body.Code != "sandbox_unavailable" {
		t.Fatalf("code = %q", body.Code)
	}
}

func TestObserveDelegation(t *testing.T) {
	server, executor, _ := newTestAgent(t, nil)
	recorder := serve(server, http.MethodGet, "/v1/executions/pp-abcdef12-12345678?executor=kubernetes&namespace=pp-exec", "agent-token", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var observation ports.ExecutionObservation
	if err := json.Unmarshal(recorder.Body.Bytes(), &observation); err != nil {
		t.Fatalf("decode observation: %v", err)
	}
	if observation.State != ports.ExecutionRunning {
		t.Fatalf("state = %s", observation.State)
	}
	ref := executor.lastObserve(t)
	if ref.ExternalID != "pp-abcdef12-12345678" || ref.Namespace != "pp-exec" || ref.Executor != "kubernetes" || ref.ClusterID != "cluster-a" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestObserveLostIsNotFound(t *testing.T) {
	server, executor, _ := newTestAgent(t, nil)
	executor.observeFn = func(context.Context, ports.ExecutionRef) (ports.ExecutionObservation, error) {
		return ports.ExecutionObservation{State: ports.ExecutionLost}, nil
	}
	recorder := serve(server, http.MethodGet, "/v1/executions/pp-gone", "agent-token", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := decodeProblem(t, recorder); body.Code != "not_found" {
		t.Fatalf("code = %q", body.Code)
	}
}

func TestCancelDelegation(t *testing.T) {
	server, executor, _ := newTestAgent(t, nil)
	recorder := serve(server, http.MethodDelete, "/v1/executions/pp-abcdef12-12345678?namespace=pp-exec", "agent-token", nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if executor.cancelCount() != 1 {
		t.Fatalf("cancel calls = %d", executor.cancelCount())
	}
}

func TestInvalidPathAndMethod(t *testing.T) {
	server, _, _ := newTestAgent(t, nil)

	recorder := serve(server, http.MethodGet, "/v1/executions/not%20valid", "agent-token", nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d", recorder.Code)
	}

	recorder = serve(server, http.MethodGet, "/v1/executions", "agent-token", nil)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", recorder.Code)
	}
	if body := decodeProblem(t, recorder); body.Code != "method_not_allowed" {
		t.Fatalf("code = %q", body.Code)
	}

	recorder = serve(server, http.MethodGet, "/unknown", "agent-token", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", recorder.Code)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	server, _, _ := newTestAgent(t, nil)
	payload := strings.NewReader(strings.Repeat("a", maxBodyBytes+1024))
	recorder := serve(server, http.MethodPost, "/v1/executions", "agent-token", payload)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if body := decodeProblem(t, recorder); body.Code != "request_too_large" {
		t.Fatalf("code = %q", body.Code)
	}
}

func TestRequestIDPropagation(t *testing.T) {
	server, _, _ := newTestAgent(t, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	request.Header.Set("X-Request-Id", "req-abc123")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if got := recorder.Header().Get("X-Request-Id"); got != "req-abc123" {
		t.Fatalf("request id = %q", got)
	}

	request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-Id", "bad header value")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	generated := recorder.Header().Get("X-Request-Id")
	if generated == "" || generated == "bad header value" || !requestIDPattern.MatchString(generated) {
		t.Fatalf("generated request id = %q", generated)
	}
}

func TestHeartbeatAndShutdown(t *testing.T) {
	server, _, registrar := newTestAgent(t, func(cfg *Config) {
		cfg.HeartbeatInterval = 5 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()

	waitFor(t, 2*time.Second, func() bool { return server.Addr() != "" })

	waitFor(t, 2*time.Second, func() bool { return registrar.count() >= 2 })
	cluster := registrar.last(t)
	if !cluster.Healthy {
		t.Fatal("heartbeat must mark the cluster healthy")
	}
	if cluster.LastHeartbeat.IsZero() {
		t.Fatal("heartbeat must set the last heartbeat time")
	}
	if cluster.ExecutorKind != "remote" {
		t.Fatalf("executor kind = %q", cluster.ExecutorKind)
	}
	if len(cluster.Languages) != 1 || cluster.Languages[0] != "go" {
		t.Fatalf("languages = %v", cluster.Languages)
	}
	if len(cluster.Engines) != 1 || cluster.Engines[0] != "go-analyzer" {
		t.Fatalf("engines = %v", cluster.Engines)
	}
	if len(cluster.IsolationTiers) != 1 || cluster.IsolationTiers[0] != domain.IsolationContainer {
		t.Fatalf("isolation tiers = %v", cluster.IsolationTiers)
	}
	if cluster.MaxConcurrentSteps != 5 {
		t.Fatalf("max concurrent = %d", cluster.MaxConcurrentSteps)
	}
	if cluster.Labels["features"] != "callback" {
		t.Fatalf("features label = %q", cluster.Labels["features"])
	}
	if cluster.ID != "cluster-a" || cluster.Region != "region-1" {
		t.Fatalf("cluster identity = %+v", cluster)
	}

	response, err := http.Get("http://" + server.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", response.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop after cancellation")
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestExplicitShutdown(t *testing.T) {
	server, _, _ := newTestAgent(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()

	waitFor(t, 2*time.Second, func() bool { return server.Addr() != "" })

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}

func TestMergeCapabilitiesReportsCapacityAndDepth(t *testing.T) {
	cluster := domain.Cluster{ID: "cluster-a", Region: "region-1"}
	mergeCapabilities(&cluster, ports.ExecutorCapabilities{
		ExecutorKind: "kubernetes", MaxConcurrent: 8,
		AvailableCPUMilli: 32000, AvailableMemoryMiB: 65536,
		ActiveAttempts: 3, QueueDepth: 2,
	})
	if cluster.ExecutorKind != "remote" {
		t.Fatalf("executor kind = %q, want remote", cluster.ExecutorKind)
	}
	if cluster.AvailableCPUMilli != 32000 || cluster.AvailableMemoryMiB != 65536 {
		t.Fatalf("capacity = %d/%d", cluster.AvailableCPUMilli, cluster.AvailableMemoryMiB)
	}
	if cluster.ActiveAttempts != 3 || cluster.QueueDepth != 2 {
		t.Fatalf("depth = %d/%d", cluster.ActiveAttempts, cluster.QueueDepth)
	}
}
