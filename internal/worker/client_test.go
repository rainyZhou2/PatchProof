package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	tenantID  = domain.TenantID("11111111-1111-4111-8111-111111111111")
	runID     = domain.RunID("22222222-2222-4222-8222-222222222222")
	stepID    = domain.StepID("33333333-3333-4333-8333-333333333333")
	attemptID = domain.AttemptID("44444444-4444-4444-8444-444444444444")
)

func testRef() domain.AttemptRef {
	return domain.AttemptRef{TenantID: tenantID, RunID: runID, StepID: stepID, AttemptID: attemptID, Generation: 2}
}

func TestClientReportsAttemptLifecycle(t *testing.T) {
	var paths []string
	var completed domain.StepResult
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		authHeader = r.Header.Get("Authorization")
		if r.URL.Path == "/internal/v1/attempts/"+attemptID.String()+":complete" {
			var body struct {
				Generation int64             `json:"generation"`
				Result     domain.StepResult `json:"result"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			completed = body.Result
			if body.Generation != 2 {
				t.Errorf("generation = %d", body.Generation)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := testRef()
	if err := client.Start(ctx, ref, "token-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(ctx, ref, "token-1"); err != nil {
		t.Fatal(err)
	}
	result := domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + "a" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FinishedAt: time.Now().UTC()}
	if err := client.Complete(ctx, ref, "token-1", result); err != nil {
		t.Fatal(err)
	}
	if err := client.Fail(ctx, ref, "token-1", domain.ErrTimeout, "timed out", true); err != nil {
		t.Fatal(err)
	}
	if authHeader != "Bearer token-1" {
		t.Fatalf("authorization header = %q", authHeader)
	}
	if completed.ResultDigest != result.ResultDigest {
		t.Fatalf("completed result digest = %q", completed.ResultDigest)
	}
	if len(paths) != 4 {
		t.Fatalf("expected four callbacks, got %v", paths)
	}
}

func TestClientRetriesServerErrors(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(context.Background(), testRef(), "token"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() < 2 {
		t.Fatalf("expected a retry, got %d attempts", attempts.Load())
	}
}

func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"INVALID_REQUEST"}`)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Heartbeat(context.Background(), testRef(), "token"); err == nil {
		t.Fatal("expected an error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("client error was retried: %d attempts", attempts.Load())
	}
}
