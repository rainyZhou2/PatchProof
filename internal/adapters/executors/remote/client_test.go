package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

func newTestExecutor(t *testing.T, handler http.HandlerFunc) *Executor {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	executor, err := New(Config{
		Endpoint:  server.URL,
		Token:     "agent-token",
		ClusterID: "cluster-a",
		Timeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return executor
}

func TestCapabilities(t *testing.T) {
	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/capabilities" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Request-Id") == "" {
			t.Error("request id header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ports.ExecutorCapabilities{
			Name:           "agent",
			ExecutorKind:   "kubernetes",
			Languages:      []string{"go"},
			IsolationTiers: []domain.IsolationTier{domain.IsolationContainer},
		})
	})
	capabilities, err := executor.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if capabilities.Name != "agent" || capabilities.ExecutorKind != "kubernetes" {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestStart(t *testing.T) {
	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/executions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body executionRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.AttemptToken != "attempt-token" || body.CallbackURL != "https://controller.test/callback" {
			t.Errorf("credentials not forwarded: %+v", body)
		}
		if body.Spec.AttemptID == "" {
			t.Error("spec attempt id missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ports.ExecutionRef{
			Executor:   "kubernetes",
			Namespace:  "pp-exec",
			ExternalID: "pp-abcdef12-12345678",
		})
	})

	spec := domain.StepSpec{
		RunID:      domain.NewRunID(),
		StepID:     domain.NewStepID(),
		AttemptID:  domain.NewAttemptID(),
		TenantID:   domain.NewTenantID(),
		Generation: 1,
		Deadline:   time.Now().Add(time.Minute),
		Requirements: domain.ResourceRequirements{
			Architecture:    "amd64",
			Isolation:       domain.IsolationContainer,
			CPUMilli:        100,
			MemoryMiB:       64,
			Network:         domain.NetworkNone,
			DeadlineSeconds: 60,
		},
	}
	ref, err := executor.Start(context.Background(), ports.ExecutionRequest{
		Spec:         spec,
		AttemptToken: "attempt-token",
		CallbackURL:  "https://controller.test/callback",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ref.ExternalID != "pp-abcdef12-12345678" || ref.Executor != "kubernetes" || ref.Namespace != "pp-exec" {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.ClusterID != "cluster-a" {
		t.Fatalf("cluster id = %q, want cluster-a", ref.ClusterID)
	}
}

func TestStartRejectsEmptyExternalID(t *testing.T) {
	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ports.ExecutionRef{})
	})
	_, err := executor.Start(context.Background(), ports.ExecutionRequest{
		Spec: domain.StepSpec{Requirements: domain.ResourceRequirements{Isolation: domain.IsolationContainer}},
	})
	if err == nil {
		t.Fatal("expected empty external id error")
	}
}

func TestObserve(t *testing.T) {
	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/executions/pp-abcdef12-12345678" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("executor") != "kubernetes" || r.URL.Query().Get("namespace") != "pp-exec" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ports.ExecutionObservation{
			State:  ports.ExecutionCompleted,
			Result: &domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:abc"},
		})
	})
	observation, err := executor.Observe(context.Background(), ports.ExecutionRef{
		ExternalID: "pp-abcdef12-12345678",
		Executor:   "kubernetes",
		Namespace:  "pp-exec",
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observation.State != ports.ExecutionCompleted || observation.Result == nil {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestObserveNotFoundIsLost(t *testing.T) {
	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404,"detail":"execution not found","code":"not_found"}`))
	})
	observation, err := executor.Observe(context.Background(), ports.ExecutionRef{ExternalID: "pp-gone"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observation.State != ports.ExecutionLost || observation.ErrorClass != domain.ErrWorkerLost {
		t.Fatalf("observation = %+v, want LOST", observation)
	}
}

func TestObserveServerErrorIsRetryable(t *testing.T) {
	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Bad Gateway","status":502,"detail":"job API unavailable","code":"executor_error"}`))
	})
	_, err := executor.Observe(context.Background(), ports.ExecutionRef{ExternalID: "pp-abcdef12"})
	if err == nil {
		t.Fatal("expected error")
	}
	var retryable *RetryableError
	if !errors.As(err, &retryable) || !retryable.Retryable() {
		t.Fatalf("error = %v, want retryable", err)
	}
	if !strings.Contains(err.Error(), "job API unavailable") {
		t.Fatalf("error = %v, want problem detail", err)
	}
}

func TestAuthErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"invalid bearer token","code":"unauthorized"}`))
		})
		_, err := executor.Capabilities(context.Background())
		if err == nil {
			t.Fatalf("status %d: expected error", status)
		}
		if status == http.StatusUnauthorized && !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("status %d: error = %v", status, err)
		}
		if status == http.StatusForbidden && !errors.Is(err, ErrForbidden) {
			t.Fatalf("status %d: error = %v", status, err)
		}
	}
}

func TestCancel(t *testing.T) {
	calls := 0
	executor := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/executions/pp-abcdef12-12345678" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := executor.Cancel(context.Background(), ports.ExecutionRef{ExternalID: "pp-abcdef12-12345678"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}

	notFound := newTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := notFound.Cancel(context.Background(), ports.ExecutionRef{ExternalID: "pp-gone"}); err != nil {
		t.Fatalf("Cancel missing: %v", err)
	}
}

func TestTrailingSlashEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capabilities" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"agent","executor_kind":"kubernetes"}`))
	}))
	defer server.Close()
	executor, err := New(Config{Endpoint: server.URL + "/", Token: "token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := executor.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
}

func TestTLSConfig(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"agent","executor_kind":"kubernetes"}`))
	}))
	defer server.Close()
	executor, err := New(Config{
		Endpoint:  server.URL,
		Token:     "token",
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := executor.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities over TLS: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "empty endpoint", cfg: Config{Token: "token"}},
		{name: "relative endpoint", cfg: Config{Endpoint: "/v1", Token: "token"}},
		{name: "bad scheme", cfg: Config{Endpoint: "ftp://agent.test", Token: "token"}},
		{name: "empty token", cfg: Config{Endpoint: "https://agent.test"}},
		{name: "tls with http", cfg: Config{Endpoint: "http://agent.test", Token: "token", TLSConfig: &tls.Config{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.cfg); err == nil {
				t.Fatalf("expected error for %s", test.name)
			}
		})
	}
}
