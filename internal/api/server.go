package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/patchproof/patchproof/internal/app/admin"
	"github.com/patchproof/patchproof/internal/app/attempts"
	"github.com/patchproof/patchproof/internal/app/runservice"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/internal/security/auth"
	"github.com/patchproof/patchproof/internal/security/ratelimit"
)

type RunService interface {
	Submit(ctx context.Context, input runservice.SubmitInput) (domain.Run, error)
	Get(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Run, error)
	List(ctx context.Context, filter domain.RunFilter) (domain.RunPage, error)
	DAG(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.DAG, error)
	Evidence(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Evidence, error)
	Counterexamples(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Counterexample, error)
	Report(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Report, error)
	Cancel(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, actor string) (domain.Run, error)
}

type AdminService interface {
	CreateTenant(ctx context.Context, actor string, input admin.TenantInput) (domain.Tenant, error)
	GetTenant(ctx context.Context, tenantID domain.TenantID) (domain.Tenant, error)
	CreateProject(ctx context.Context, tenantID domain.TenantID, actor string, input admin.ProjectInput) (domain.Project, error)
	GetProject(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID) (domain.Project, error)
	PutPolicy(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID, actor string, raw []byte) (domain.Policy, error)
	ValidatePolicy(raw []byte) (admin.PolicyValidation, error)
	ListClusters(ctx context.Context, region string) ([]domain.Cluster, error)
}

type Deps struct {
	Runs          RunService
	Admin         AdminService
	Attempts      *attempts.Service
	Tokens        ports.AttemptTokens
	Authenticator auth.Authenticator
	Reporters     map[string]ports.Reporter
	Metrics       *telemetry.Metrics
	Limiter       *ratelimit.Limiter
	Logger        *slog.Logger
	Readiness     func(context.Context) error
	PublicURL     string
	Version       string
	Region        string
	MaxBodyBytes  int64
}

type Server struct {
	deps    Deps
	handler http.Handler
}

func New(deps Deps) (*Server, error) {
	if deps.Runs == nil || deps.Admin == nil || deps.Attempts == nil || deps.Tokens == nil || deps.Authenticator == nil {
		return nil, fmt.Errorf("api: runs, admin, attempts, tokens and authenticator are required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.MaxBodyBytes <= 0 {
		deps.MaxBodyBytes = 2 << 20
	}
	if deps.Reporters == nil {
		deps.Reporters = map[string]ports.Reporter{}
	}
	server := &Server{deps: deps}
	mux := http.NewServeMux()
	server.routes(mux)
	server.handler = server.wrap(mux)
	return server, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)

	mux.HandleFunc("POST /api/v1/runs", s.handleCreateRun)
	mux.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	mux.HandleFunc("GET /api/v1/runs/{run_id}", s.handleGetRun)
	mux.HandleFunc("GET /api/v1/runs/{run_id}/steps", s.handleRunSteps)
	mux.HandleFunc("POST /api/v1/runs/{run_id}/cancel", s.handleCancelRun)
	mux.HandleFunc("GET /api/v1/runs/{run_id}/report", s.handleReport)
	mux.HandleFunc("GET /api/v1/runs/{run_id}/evidence", s.handleEvidence)
	mux.HandleFunc("GET /api/v1/runs/{run_id}/counterexamples", s.handleCounterexamples)
	mux.HandleFunc("POST /api/v1/reproductions", s.handleReproduction)

	mux.HandleFunc("POST /api/v1/projects", s.handleCreateProject)
	mux.HandleFunc("GET /api/v1/projects/{project_id}", s.handleGetProject)
	mux.HandleFunc("POST /api/v1/policies", s.handlePutPolicy)
	mux.HandleFunc("POST /api/v1/policies/validate", s.handleValidatePolicy)
	mux.HandleFunc("GET /api/v1/clusters", s.handleListClusters)
	mux.HandleFunc("POST /api/v1/tenants", s.handleCreateTenant)
	mux.HandleFunc("GET /api/v1/tenants/{tenant_id}", s.handleGetTenant)

	mux.HandleFunc("POST /internal/v1/attempts/{attempt_id}/start", s.handleAttemptStart)
	mux.HandleFunc("POST /internal/v1/attempts/{attempt_id}/heartbeat", s.handleAttemptHeartbeat)
	mux.HandleFunc("POST /internal/v1/attempts/{attempt_id}/complete", s.handleAttemptComplete)
	mux.HandleFunc("POST /internal/v1/attempts/{attempt_id}/fail", s.handleAttemptFail)
}

func (s *Server) wrap(next http.Handler) http.Handler {
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withRequestID(r.Context(), incomingRequestID(r)))
		s.serve(w, r, next)
	})
	handler = s.recoverer(handler)
	if s.deps.Metrics != nil {
		handler = telemetry.InstrumentHTTP(s.deps.Metrics, handler)
	}
	handler = telemetry.TraceHTTP("patchproof-api", handler)
	return handler
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	started := time.Now()
	recorder := &responseRecorder{ResponseWriter: w}
	defer func() {
		s.deps.Logger.Info("http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.statusCode()),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()),
			slog.String("request_id", requestIDFrom(r.Context())),
		)
	}()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, s.deps.MaxBodyBytes)
	normalizeActionPath(r)

	if strings.HasPrefix(r.URL.Path, "/api/") {
		principal, err := s.deps.Authenticator.Authenticate(r.Context(), r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="patchproof"`)
			writeError(recorder, r, err)
			return
		}
		r = r.WithContext(withPrincipal(r.Context(), principal))
		if s.deps.Limiter != nil && !s.deps.Limiter.Allow(principal.TenantID) {
			w.Header().Set("Retry-After", "1")
			writeProblem(recorder, r, http.StatusTooManyRequests, "RATE_LIMITED", "tenant request rate exceeded")
			return
		}
	}
	next.ServeHTTP(recorder, r)
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.deps.Logger.Error("handler panic",
					slog.String("path", r.URL.Path), slog.String("request_id", requestIDFrom(r.Context())), slog.Any("panic", recovered))
				writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.deps.Readiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.deps.Readiness(ctx); err != nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "NOT_READY", "dependencies are not ready")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := principalFrom(r.Context()); !ok {
		writeProblem(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication is required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": s.deps.Version, "region": s.deps.Region})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(data)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func incomingRequestID(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if value != "" && len(value) <= 128 {
		safe := true
		for _, char := range value {
			if !(char == '-' || char == '_' || char == '.' || (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
				safe = false
				break
			}
		}
		if safe {
			return value
		}
	}
	return uuid.NewString()
}

func normalizeActionPath(r *http.Request) {
	path := r.URL.Path
	if !strings.Contains(path, ":") {
		return
	}
	if index := strings.LastIndex(path, "/"); index >= 0 {
		segment := path[index+1:]
		if colon := strings.Index(segment, ":"); colon > 0 {
			r.URL.Path = path[:index+1] + segment[:colon] + "/" + segment[colon+1:]
		}
	}
}
