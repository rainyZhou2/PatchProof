// Package clusteragent exposes a cluster executor over an authenticated HTTP
// API and keeps the cluster's registration fresh. It is the server side of the
// protocol implemented by internal/adapters/executors/remote.
package clusteragent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	defaultListenAddr        = ":8080"
	defaultHeartbeatInterval = 30 * time.Second
	defaultReadHeaderTimeout = 5 * time.Second
	serverReadTimeout        = 30 * time.Second
	serverWriteTimeout       = 2 * time.Minute
	serverIdleTimeout        = 2 * time.Minute
	serverMaxHeaderBytes     = 1 << 20
	maxBodyBytes             = 1 << 20
	heartbeatTimeout         = 10 * time.Second
	readinessTimeout         = 3 * time.Second
	shutdownTimeout          = 20 * time.Second
	maxMessageLength         = 512
	requestIDHeader          = "X-Request-Id"
	problemTypePrefix        = "https://patchproof.dev/problems/"
	executorErrorCode        = "executor_error"
)

var (
	executionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	namespacePattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	requestIDPattern   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
)

// Registrar persists cluster heartbeats. The control-plane metadata store
// implements it.
type Registrar interface {
	UpsertCluster(ctx context.Context, cluster domain.Cluster) error
}

// Config configures the cluster agent.
type Config struct {
	// ListenAddr is the TCP address the HTTP API binds to.
	ListenAddr string
	// Token authenticates control-plane callers.
	Token string
	// Executor serves the execution API.
	Executor ports.Executor
	// Registrar receives periodic heartbeats.
	Registrar Registrar
	// HeartbeatInterval is the cluster registration cadence.
	HeartbeatInterval time.Duration
	// ClusterInfo is the base record enriched with executor capabilities.
	ClusterInfo domain.Cluster
	// ReadHeaderTimeout bounds request header reads.
	ReadHeaderTimeout time.Duration
	// Logger overrides the default structured logger.
	Logger *slog.Logger
}

// Server is the HTTP and heartbeat process for one execution cluster.
type Server struct {
	cfg       Config
	logger    *slog.Logger
	handler   http.Handler
	tokenHash [sha256.Size]byte

	mu         sync.Mutex
	httpServer *http.Server
	listener   net.Listener
	started    bool
	hbCancel   context.CancelFunc
	hbWG       sync.WaitGroup
}

// New validates cfg and builds the agent. It does not bind a socket or contact
// the metadata store.
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("cluster agent: bearer token is required")
	}
	if cfg.Executor == nil {
		return nil, errors.New("cluster agent: executor is required")
	}
	if cfg.Registrar == nil {
		return nil, errors.New("cluster agent: registrar is required")
	}
	if strings.TrimSpace(string(cfg.ClusterInfo.ID)) == "" {
		return nil, errors.New("cluster agent: cluster info must carry an id")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeatInterval
	}
	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{cfg: cfg, logger: logger}
	server.tokenHash = sha256.Sum256([]byte(cfg.Token))
	server.handler = server.buildHandler()
	return server, nil
}

// Handler returns the HTTP handler. It is exposed for embedding and tests.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr returns the bound listener address, or an empty string before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Start runs the heartbeat loop and serves the HTTP API until ctx is cancelled
// or the listener fails. It performs a graceful shutdown on cancellation.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("cluster agent: already started")
	}
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("cluster agent: listen on %s: %w", s.cfg.ListenAddr, err)
	}
	heartbeatCtx, hbCancel := context.WithCancel(context.Background())
	httpServer := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverMaxHeaderBytes,
	}
	s.httpServer = httpServer
	s.listener = listener
	s.started = true
	s.hbCancel = hbCancel
	s.hbWG.Add(1)
	s.mu.Unlock()

	go s.heartbeatLoop(heartbeatCtx)

	serveErr := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-serveErr
	case err := <-serveErr:
		hbCancel()
		return err
	}
}

// Shutdown stops the heartbeat loop and gracefully drains the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	hbCancel := s.hbCancel
	httpServer := s.httpServer
	s.hbCancel = nil
	s.mu.Unlock()

	if hbCancel != nil {
		hbCancel()
	}
	var shutdownErr error
	if httpServer != nil {
		shutdownErr = httpServer.Shutdown(ctx)
	}

	done := make(chan struct{})
	go func() {
		s.hbWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if shutdownErr == nil {
			shutdownErr = ctx.Err()
		}
	}
	return shutdownErr
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.getOnly(s.handleHealth))
	mux.HandleFunc("/readyz", s.getOnly(s.handleReady))
	mux.HandleFunc("/v1/capabilities", s.requireAuth(s.getOnly(s.handleCapabilities)))
	mux.HandleFunc("/v1/executions", s.requireAuth(s.handleExecutions))
	mux.HandleFunc("/v1/executions/{id}", s.requireAuth(s.handleExecution))
	mux.HandleFunc("/", s.handleNotFound)
	return s.withRequestID(s.withLogging(mux))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	capabilities, err := s.cfg.Executor.Capabilities(ctx)
	if err != nil {
		s.logError(r, "readiness check failed", err)
		s.writeProblem(w, http.StatusServiceUnavailable, "not_ready", "executor capabilities are unavailable")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "executor": capabilities.Name})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	capabilities, err := s.cfg.Executor.Capabilities(r.Context())
	if err != nil {
		s.executorFailure(w, r, "capabilities", err)
		return
	}
	s.writeJSON(w, http.StatusOK, capabilities)
}

func (s *Server) handleExecutions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}
	if r.ContentLength > maxBodyBytes {
		s.writeProblem(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 1 MiB")
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var request executionRequest
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			s.writeProblem(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 1 MiB")
			return
		}
		s.writeProblem(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if err := validateExecutionRequest(request); err != nil {
		s.writeProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ref, err := s.cfg.Executor.Start(r.Context(), request.toPorts())
	if err != nil {
		s.executorFailure(w, r, "start", err)
		return
	}
	s.writeJSON(w, http.StatusCreated, ref)
}

func (s *Server) handleExecution(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleObserve(w, r)
	case http.MethodDelete:
		s.handleCancel(w, r)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodDelete)
		s.writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET and DELETE are supported")
	}
}

func (s *Server) handleObserve(w http.ResponseWriter, r *http.Request) {
	ref, err := s.executionRef(r)
	if err != nil {
		s.writeProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	observation, err := s.cfg.Executor.Observe(r.Context(), ref)
	if err != nil {
		s.executorFailure(w, r, "observe", err)
		return
	}
	if observation.State == ports.ExecutionLost {
		s.writeProblem(w, http.StatusNotFound, "not_found", "execution is unknown to the agent")
		return
	}
	s.writeJSON(w, http.StatusOK, observation)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	ref, err := s.executionRef(r)
	if err != nil {
		s.writeProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.cfg.Executor.Cancel(r.Context(), ref); err != nil {
		s.executorFailure(w, r, "cancel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeProblem(w, http.StatusNotFound, "not_found", "no route matches "+r.Method+" "+r.URL.Path)
}

func (s *Server) executionRef(r *http.Request) (ports.ExecutionRef, error) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !executionIDPattern.MatchString(id) {
		return ports.ExecutionRef{}, errors.New("invalid execution id")
	}
	query := r.URL.Query()
	executor := strings.TrimSpace(query.Get("executor"))
	if len(executor) > 64 {
		return ports.ExecutionRef{}, errors.New("executor name is too long")
	}
	namespace := strings.TrimSpace(query.Get("namespace"))
	if namespace != "" && (len(namespace) > 63 || !namespacePattern.MatchString(namespace)) {
		return ports.ExecutionRef{}, errors.New("invalid namespace")
	}
	return ports.ExecutionRef{
		Executor:   executor,
		ClusterID:  s.cfg.ClusterInfo.ID,
		Namespace:  namespace,
		ExternalID: id,
	}, nil
}

func (s *Server) executorFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	status := http.StatusBadGateway
	code := executorErrorCode
	var classified interface{ ErrorClass() domain.ErrorClass }
	if errors.As(err, &classified) {
		switch classified.ErrorClass() {
		case domain.ErrValidationFailed:
			status, code = http.StatusBadRequest, "invalid_request"
		case domain.ErrSandboxUnavailable:
			status, code = http.StatusServiceUnavailable, "sandbox_unavailable"
		case domain.ErrCancelled:
			status, code = http.StatusConflict, "cancelled"
		}
	}
	var validation *domain.ValidationError
	if errors.As(err, &validation) {
		status, code = http.StatusBadRequest, "invalid_request"
	}
	s.logError(r, "executor "+operation+" failed", err)
	s.writeProblem(w, status, code, truncate(err.Error(), maxMessageLength))
}

func (s *Server) getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			s.writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="patchproof-cluster-agent"`)
			s.writeProblem(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	presented := sha256.Sum256([]byte(strings.TrimSpace(header[len(prefix):])))
	return subtle.ConstantTimeCompare(presented[:], s.tokenHash[:]) == 1
}

type requestIDKey struct{}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		logger := s.logger.Info
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			logger = s.logger.Debug
		}
		logger("cluster agent request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.Duration("duration", time.Since(started)),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(payload []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(payload)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		s.logger.Warn("cluster agent: encode response", slog.String("error", err.Error()))
	}
}

type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
}

func (s *Server) writeProblem(w http.ResponseWriter, status int, code, detail string) {
	title := http.StatusText(status)
	if title == "" {
		title = "Error"
	}
	body := problem{
		Type:   problemTypePrefix + code,
		Title:  title,
		Status: status,
		Detail: truncate(detail, maxMessageLength),
		Code:   code,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Warn("cluster agent: encode problem", slog.String("error", err.Error()))
	}
}

func (s *Server) logError(r *http.Request, message string, err error) {
	s.logger.Warn(message,
		slog.String("request_id", requestIDFrom(r.Context())),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("error", err.Error()),
	)
}

func (s *Server) heartbeatLoop(ctx context.Context) {
	defer s.hbWG.Done()
	s.heartbeat(ctx)
	ticker := time.NewTicker(s.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.heartbeat(ctx)
		}
	}
}

func (s *Server) heartbeat(parent context.Context) {
	timeout := s.cfg.HeartbeatInterval
	if timeout <= 0 || timeout > heartbeatTimeout {
		timeout = heartbeatTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cluster := s.cfg.ClusterInfo
	cluster.Labels = maps.Clone(s.cfg.ClusterInfo.Labels)
	cluster.LastHeartbeat = time.Now().UTC()
	capabilities, err := s.cfg.Executor.Capabilities(ctx)
	if err != nil {
		cluster.Healthy = false
		s.logger.Warn("cluster agent: capabilities unavailable", slog.String("error", err.Error()))
		if upsertErr := s.cfg.Registrar.UpsertCluster(ctx, cluster); upsertErr != nil {
			s.logger.Warn("cluster agent: heartbeat failed", slog.String("error", upsertErr.Error()))
		}
		return
	}
	mergeCapabilities(&cluster, capabilities)
	cluster.Healthy = true
	if err := s.cfg.Registrar.UpsertCluster(ctx, cluster); err != nil {
		s.logger.Warn("cluster agent: heartbeat failed", slog.String("error", err.Error()))
	}
}

func mergeCapabilities(cluster *domain.Cluster, capabilities ports.ExecutorCapabilities) {
	// A cluster agent always executes over the remote transport. The executor
	// it wraps reports its local kind (for example "kubernetes"); overwriting
	// the registration with that value makes the control plane believe it owns
	// the cluster and bypass the agent. Keep an explicitly declared kind, and
	// otherwise register the agent as a remote executor.
	if cluster.ExecutorKind == "" {
		cluster.ExecutorKind = "remote"
	}
	if cluster.Region == "" && capabilities.Region != "" {
		cluster.Region = capabilities.Region
	}
	if len(capabilities.Architectures) > 0 {
		cluster.Architectures = append([]string(nil), capabilities.Architectures...)
	}
	if len(capabilities.Languages) > 0 {
		cluster.Languages = append([]string(nil), capabilities.Languages...)
	}
	if len(capabilities.Engines) > 0 {
		cluster.Engines = append([]string(nil), capabilities.Engines...)
	}
	if len(capabilities.IsolationTiers) > 0 {
		cluster.IsolationTiers = append([]domain.IsolationTier(nil), capabilities.IsolationTiers...)
	}
	if len(capabilities.RuntimeClasses) > 0 {
		cluster.RuntimeClasses = append([]string(nil), capabilities.RuntimeClasses...)
	}
	if capabilities.MaxConcurrent > 0 {
		cluster.MaxConcurrentSteps = capabilities.MaxConcurrent
	}
	if capabilities.AvailableCPUMilli > 0 {
		cluster.AvailableCPUMilli = capabilities.AvailableCPUMilli
	}
	if capabilities.AvailableMemoryMiB > 0 {
		cluster.AvailableMemoryMiB = capabilities.AvailableMemoryMiB
	}
	if capabilities.ActiveAttempts >= 0 {
		cluster.ActiveAttempts = capabilities.ActiveAttempts
	}
	if capabilities.QueueDepth >= 0 {
		cluster.QueueDepth = capabilities.QueueDepth
	}
	if len(capabilities.Features) > 0 {
		if cluster.Labels == nil {
			cluster.Labels = map[string]string{}
		}
		cluster.Labels["features"] = strings.Join(capabilities.Features, ",")
	}
}

func validateExecutionRequest(request executionRequest) error {
	if request.AttemptToken == "" {
		return errors.New("attempt_token is required")
	}
	if len(request.AttemptToken) > 8192 {
		return errors.New("attempt_token is too long")
	}
	if request.CallbackURL != "" {
		parsed, err := url.Parse(request.CallbackURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New("callback_url must be an absolute HTTP(S) URL")
		}
	}
	if err := request.Spec.RunID.Validate(); err != nil {
		return err
	}
	if err := request.Spec.StepID.Validate(); err != nil {
		return err
	}
	if err := request.Spec.AttemptID.Validate(); err != nil {
		return err
	}
	if err := request.Spec.TenantID.Validate(); err != nil {
		return err
	}
	return request.Spec.Requirements.Validate()
}

type executionRequest struct {
	Spec         domain.StepSpec `json:"spec"`
	AttemptToken string          `json:"attempt_token"`
	CallbackURL  string          `json:"callback_url"`
	WorkspaceRef domain.CASRef   `json:"workspace_ref"`
}

func (r executionRequest) toPorts() ports.ExecutionRequest {
	return ports.ExecutionRequest{
		Spec:         r.Spec,
		AttemptToken: r.AttemptToken,
		CallbackURL:  r.CallbackURL,
		WorkspaceRef: r.WorkspaceRef,
	}
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
