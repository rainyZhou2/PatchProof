package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/app/admin"
	"github.com/patchproof/patchproof/internal/app/attempts"
	"github.com/patchproof/patchproof/internal/app/runservice"
	"github.com/patchproof/patchproof/internal/app/tokens"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/internal/security/auth"
)

const (
	testToken  = "test-token"
	testTenant = domain.TenantID("11111111-1111-4111-8111-111111111111")
)

type staticAuth struct {
	principal auth.Principal
	err       error
}

func (a staticAuth) Authenticate(context.Context, *http.Request) (auth.Principal, error) {
	return a.principal, a.err
}

type fakeReporter struct {
	contentType string
}

func (r fakeReporter) Format() string { return "markdown" }
func (r fakeReporter) ContentType() string {
	if r.contentType == "" {
		return "text/markdown"
	}
	return r.contentType
}
func (r fakeReporter) Render(context.Context, domain.Report, []domain.Evidence) ([]byte, error) {
	return []byte("# report\n"), nil
}

type fakeRuns struct {
	submitted []runservice.SubmitInput
	cancelled bool
	run       domain.Run
	report    domain.Report
	err       error
}

func (f *fakeRuns) Submit(_ context.Context, input runservice.SubmitInput) (domain.Run, error) {
	f.submitted = append(f.submitted, input)
	if f.err != nil {
		return domain.Run{}, f.err
	}
	return f.run, nil
}

func (f *fakeRuns) Get(context.Context, domain.TenantID, domain.RunID) (domain.Run, error) {
	return f.run, f.err
}
func (f *fakeRuns) List(context.Context, domain.RunFilter) (domain.RunPage, error) {
	return domain.RunPage{Runs: []domain.Run{f.run}}, f.err
}
func (f *fakeRuns) DAG(context.Context, domain.TenantID, domain.RunID) (domain.DAG, error) {
	return domain.DAG{RunID: f.run.ID, Steps: []domain.Step{{ID: "33333333-3333-4333-8333-333333333333", RunID: f.run.ID, TenantID: testTenant, Key: "compile", Kind: domain.StepKindCompile, Status: domain.StepReady}}}, f.err
}
func (f *fakeRuns) Evidence(context.Context, domain.TenantID, domain.RunID) ([]domain.Evidence, error) {
	return nil, f.err
}
func (f *fakeRuns) Counterexamples(context.Context, domain.TenantID, domain.RunID) ([]domain.Counterexample, error) {
	return nil, f.err
}
func (f *fakeRuns) Report(context.Context, domain.TenantID, domain.RunID) (domain.Report, error) {
	return f.report, f.err
}
func (f *fakeRuns) Cancel(context.Context, domain.TenantID, domain.RunID, string) (domain.Run, error) {
	f.cancelled = true
	return f.run, f.err
}

type fakeAdmin struct{}

func (fakeAdmin) CreateTenant(context.Context, string, admin.TenantInput) (domain.Tenant, error) {
	return domain.Tenant{ID: testTenant}, nil
}
func (fakeAdmin) GetTenant(context.Context, domain.TenantID) (domain.Tenant, error) {
	return domain.Tenant{ID: testTenant}, nil
}
func (fakeAdmin) CreateProject(context.Context, domain.TenantID, string, admin.ProjectInput) (domain.Project, error) {
	return domain.Project{ID: "55555555-5555-4555-8555-555555555555"}, nil
}
func (fakeAdmin) GetProject(context.Context, domain.TenantID, domain.ProjectID) (domain.Project, error) {
	return domain.Project{ID: "55555555-5555-4555-8555-555555555555"}, nil
}
func (fakeAdmin) PutPolicy(context.Context, domain.TenantID, domain.ProjectID, string, []byte) (domain.Policy, error) {
	return domain.Policy{Digest: "sha256:abc"}, nil
}
func (fakeAdmin) ValidatePolicy([]byte) (admin.PolicyValidation, error) {
	return admin.PolicyValidation{Digest: "sha256:abc"}, nil
}
func (fakeAdmin) ListClusters(context.Context, string) ([]domain.Cluster, error) {
	return []domain.Cluster{{ID: "local", Healthy: true}}, nil
}

func testServer(t *testing.T, principal auth.Principal, runs *fakeRuns) http.Handler {
	t.Helper()
	return testServerWithAuth(t, staticAuth{principal: principal}, runs)
}

func testServerWithAuth(t *testing.T, authenticator auth.Authenticator, runs *fakeRuns) http.Handler {
	t.Helper()
	issuer, err := tokens.NewIssuer([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	attemptService, err := attempts.New(memory.New(), nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Deps{
		Runs: runs, Admin: fakeAdmin{}, Attempts: attemptService, Tokens: issuer,
		Authenticator: authenticator,
		Reporters:     map[string]ports.Reporter{"markdown": fakeReporter{}},
		Metrics:       telemetry.NewMetrics(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		PublicURL: "https://patchproof.test", Version: "test", Region: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler()
}

func defaultPrincipal() auth.Principal {
	return auth.Principal{Subject: "tester", TenantID: testTenant, Roles: []string{"admin", "operator", "viewer"}}
}

func TestAuthenticationRequired(t *testing.T) {
	handler := testServerWithAuth(t, staticAuth{err: &auth.TokenError{Reason: "missing credentials"}}, &fakeRuns{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("expected problem+json, got %q", contentType)
	}
}

func TestCreateRunRequiresIdempotencyKey(t *testing.T) {
	principal := defaultPrincipal()
	handler := testServer(t, principal, &fakeRuns{})
	body := `{"project_id":"55555555-5555-4555-8555-555555555555","base_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","head_commit":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateRunCallsService(t *testing.T) {
	principal := defaultPrincipal()
	run := domain.Run{ID: "22222222-2222-4222-8222-222222222222", TenantID: testTenant, Status: domain.RunCreated}
	runs := &fakeRuns{run: run}
	handler := testServer(t, principal, runs)
	body := `{"project_id":"55555555-5555-4555-8555-555555555555","base_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","head_commit":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","requested_checks":["compile"],"budget":{"cpu_seconds":10,"wall_seconds":60}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "key-1")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(runs.submitted) != 1 {
		t.Fatalf("expected one submission, got %d", len(runs.submitted))
	}
	if runs.submitted[0].TenantID != testTenant || runs.submitted[0].RequestKey != "key-1" {
		t.Fatalf("unexpected submission: %#v", runs.submitted[0])
	}
	if location := recorder.Header().Get("Location"); location != "https://patchproof.test/api/v1/runs/"+run.ID.String() {
		t.Fatalf("unexpected location %q", location)
	}
}

func TestViewerCannotSubmit(t *testing.T) {
	principal := defaultPrincipal()
	principal.Roles = []string{"viewer"}
	handler := testServer(t, principal, &fakeRuns{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "key-2")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", recorder.Code)
	}
}

func TestCancelActionPathRewritten(t *testing.T) {
	run := domain.Run{ID: "22222222-2222-4222-8222-222222222222", TenantID: testTenant, Status: domain.RunCancelling}
	runs := &fakeRuns{run: run}
	handler := testServer(t, defaultPrincipal(), runs)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run.ID.String()+":cancel", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !runs.cancelled {
		t.Fatal("cancel service was not called")
	}
}

func TestReportFormatSelection(t *testing.T) {
	run := domain.Run{ID: "22222222-2222-4222-8222-222222222222", TenantID: testTenant}
	runs := &fakeRuns{run: run, report: domain.Report{RunID: run.ID}}
	handler := testServer(t, defaultPrincipal(), runs)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/report?format=markdown", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "# report") {
		t.Fatalf("unexpected body %q", recorder.Body.String())
	}
}

func TestUnknownReportFormatRejected(t *testing.T) {
	run := domain.Run{ID: "22222222-2222-4222-8222-222222222222", TenantID: testTenant}
	handler := testServer(t, defaultPrincipal(), &fakeRuns{run: run, report: domain.Report{RunID: run.ID}})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/report?format=pdf", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestInvalidStatusFilterRejected(t *testing.T) {
	handler := testServer(t, defaultPrincipal(), &fakeRuns{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs?status=NOPE", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestInternalAttemptRequiresToken(t *testing.T) {
	handler := testServer(t, defaultPrincipal(), &fakeRuns{})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/attempts/44444444-4444-4444-8444-444444444444:start", strings.NewReader(`{"generation":1}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestProblemShape(t *testing.T) {
	handler := testServer(t, defaultPrincipal(), &fakeRuns{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/not-a-uuid", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
	var document problem
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Status != http.StatusBadRequest || document.Code == "" || document.RequestID == "" {
		t.Fatalf("unexpected problem document: %#v", document)
	}
}

func TestInternalAttemptLifecycle(t *testing.T) {
	store := memory.New()
	tenant := domain.Tenant{
		ID: testTenant, HomeRegion: "local", SecurityTier: domain.SecurityTierStandard, Status: domain.TenantActive,
		ActiveWriterEpoch: 1, Quota: domain.TenantQuota{MaxConcurrentSteps: 4}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.CreateTenant(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := domain.Run{
		ID: "22222222-2222-4222-8222-222222222222", TenantID: testTenant,
		ProjectID: "55555555-5555-4555-8555-555555555555", RequestKey: "internal", Fingerprint: "fp",
		BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HeadCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		PolicyDigest: "sha256:abc", Status: domain.RunValidating, Region: "local", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.CreateRun(context.Background(), domain.CreateRunRecord{Run: run}); err != nil {
		t.Fatal(err)
	}
	step := domain.Step{
		ID: "33333333-3333-4333-8333-333333333333", RunID: run.ID, TenantID: testTenant, Key: "compile",
		Kind: domain.StepKindCompile, Status: domain.StepReady, MaxAttempts: 2, Shard: 0,
		Requirements: domain.ResourceRequirements{
			Region: "local", Architecture: "amd64", Isolation: domain.IsolationTrustedLocal, CPUMilli: 100,
			MemoryMiB: 128, EphemeralDiskGiB: 1, Network: domain.NetworkNone, DeadlineSeconds: 60,
			ArtifactTransport: domain.TransportDirect,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PlanRun(context.Background(), domain.PlanTransaction{
		TenantID: testTenant, RunID: run.ID, ExpectedVersion: 0, Steps: []domain.Step{step},
		Dependencies: map[domain.StepID][]domain.StepID{}, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetRun(context.Background(), testTenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRun(context.Background(), domain.RunUpdate{
		TenantID: testTenant, RunID: run.ID, ExpectedVersion: current.StateVersion,
		From: domain.RunPlanned, To: domain.RunQueued, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	attemptID := domain.NewAttemptID()
	lease, err := store.AcquireLease(context.Background(), domain.LeaseRequest{
		TenantID: testTenant, RunID: run.ID, StepID: step.ID, AttemptID: attemptID,
		ExpectedGeneration: 0, Owner: "test", TTL: time.Minute, ClusterID: "local", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := tokens.NewIssuer([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := issuer.Issue(context.Background(), lease.Ref(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attemptService, err := attempts.New(store, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Deps{
		Runs: &fakeRuns{}, Admin: fakeAdmin{}, Attempts: attemptService, Tokens: issuer,
		Authenticator: staticAuth{principal: defaultPrincipal()},
		Reporters:     map[string]ports.Reporter{"markdown": fakeReporter{}},
		Metrics:       telemetry.NewMetrics(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test", Region: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	start := httptest.NewRequest(http.MethodPost, "/internal/v1/attempts/"+attemptID.String()+":start", strings.NewReader(`{"generation":1}`))
	start.Header.Set("Authorization", "Bearer "+token)
	start.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, start)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("start: expected 204, got %d: %s", recorder.Code, recorder.Body.String())
	}
	result := domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + strings.Repeat("a", 64), FinishedAt: time.Now().UTC()}
	payload, err := json.Marshal(map[string]any{"generation": 1, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	complete := httptest.NewRequest(http.MethodPost, "/internal/v1/attempts/"+attemptID.String()+":complete", strings.NewReader(string(payload)))
	complete.Header.Set("Authorization", "Bearer "+token)
	complete.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, complete)
	if recorder.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored, err := store.GetStep(context.Background(), testTenant, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.StepSucceeded {
		t.Fatalf("expected SUCCEEDED, got %s", stored.Status)
	}
}
