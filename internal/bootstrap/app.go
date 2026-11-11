package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/google/uuid"

	jsonreport "github.com/patchproof/patchproof/internal/adapters/reporting/json"
	markdownreport "github.com/patchproof/patchproof/internal/adapters/reporting/markdown"
	sarifreport "github.com/patchproof/patchproof/internal/adapters/reporting/sarif"
	"github.com/patchproof/patchproof/internal/adapters/repos/git"
	"github.com/patchproof/patchproof/internal/api"
	"github.com/patchproof/patchproof/internal/app/admin"
	"github.com/patchproof/patchproof/internal/app/attempts"
	"github.com/patchproof/patchproof/internal/app/orchestrator"
	"github.com/patchproof/patchproof/internal/app/outbox"
	"github.com/patchproof/patchproof/internal/app/proof"
	"github.com/patchproof/patchproof/internal/app/runservice"
	"github.com/patchproof/patchproof/internal/app/scheduler"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/planner"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/platform/httpserver"
	"github.com/patchproof/patchproof/internal/platform/lifecycle"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/policy"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/internal/security/ratelimit"
)

type App struct {
	Config     config.Config
	Logger     *slog.Logger
	Metrics    *telemetry.Metrics
	Tracing    *telemetry.Tracing
	Store      ports.MetadataStore
	Artifacts  ports.ArtifactStore
	Bus        ports.EventBus
	API        *api.Server
	Runs       RunService
	Admin      AdminService
	Reporters  map[string]ports.Reporter
	HTTPServer *http.Server
	components []func(context.Context) error
	closers    []func() error
	readiness  func(context.Context) error
	engines    map[string]string
}

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
	CreateProject(ctx context.Context, tenantID domain.TenantID, actor string, input admin.ProjectInput) (domain.Project, error)
	PutPolicy(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID, actor string, raw []byte) (domain.Policy, error)
	ValidatePolicy(raw []byte) (admin.PolicyValidation, error)
	ListClusters(ctx context.Context, region string) ([]domain.Cluster, error)
	GetTenant(ctx context.Context, tenantID domain.TenantID) (domain.Tenant, error)
	GetProject(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID) (domain.Project, error)
}

type AppOptions struct {
	Role               config.Role
	EnableLocalCluster bool
	DisableHTTP        bool
}

func NewApp(ctx context.Context, cfg config.Config, logger *slog.Logger, metrics *telemetry.Metrics, tracing *telemetry.Tracing, options AppOptions) (*App, error) {
	role := options.Role
	if role == "" {
		role = cfg.Service.Role
	}
	if role == "" {
		role = config.RoleAllInOne
	}
	app := &App{Config: cfg, Logger: logger, Metrics: metrics, Tracing: tracing}

	store, err := OpenStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	app.Store = store
	app.closers = append(app.closers, store.Close)

	artifacts, err := OpenArtifacts(ctx, cfg)
	if err != nil {
		app.Close(context.Background())
		return nil, err
	}
	app.Artifacts = artifacts

	bus, err := OpenBus(ctx, cfg)
	if err != nil {
		app.Close(context.Background())
		return nil, err
	}
	app.Bus = bus
	app.closers = append(app.closers, bus.Close)

	tokenIssuer, err := NewTokenIssuer(cfg)
	if err != nil {
		app.Close(context.Background())
		return nil, err
	}
	app.readiness = Readiness(store, artifacts, bus)
	needsAPI := role == config.RoleAPI || role == config.RoleAllInOne || role == config.RoleProof
	needsController := role == config.RoleController || role == config.RoleAllInOne
	engineVersions, err := EngineVersions(cfg)
	if err != nil {
		app.Close(context.Background())
		return nil, err
	}
	app.engines = engineVersions
	if cfg.Auth.Mode == "dev" && needsAPI {
		if err := EnsureDevTenant(ctx, store, cfg); err != nil {
			app.Close(context.Background())
			return nil, err
		}
	}

	var runner ports.StepRunner
	if role == config.RoleAllInOne {
		engines, err := NewEngines(cfg)
		if err != nil {
			app.Close(context.Background())
			return nil, err
		}
		workerRunner, err := NewWorker(cfg, artifacts, engines, logger)
		if err != nil {
			app.Close(context.Background())
			return nil, err
		}
		runner = workerRunner
	}
	executorFactory, err := NewExecutorFactory(cfg, runner, logger)
	if err != nil {
		app.Close(context.Background())
		return nil, err
	}

	if needsController {
		if err := app.buildController(cfg, logger, metrics, executorFactory, tokenIssuer); err != nil {
			app.Close(context.Background())
			return nil, err
		}
	}
	if needsAPI {
		if err := app.buildAPI(cfg, logger, metrics, tokenIssuer); err != nil {
			app.Close(context.Background())
			return nil, err
		}
	}
	if options.EnableLocalCluster || role == config.RoleAllInOne {
		if _, err := EnsureLocalCluster(ctx, store, cfg, time.Now().UTC()); err != nil {
			app.Close(context.Background())
			return nil, err
		}
		app.components = append(app.components, LocalClusterHeartbeat(store, cfg, logger))
	}

	if !options.DisableHTTP && app.API != nil {
		server, err := httpserver.New(cfg.HTTP.ListenAddr, app.API.Handler(), cfg.HTTP, logger)
		if err != nil {
			app.Close(context.Background())
			return nil, err
		}
		app.HTTPServer = server
		app.components = append(app.components, func(ctx context.Context) error {
			return httpserver.Serve(ctx, server, logger)
		})
	}
	if cfg.Telemetry.MetricsAddr != "" {
		app.components = append(app.components, app.metricsServer(cfg, logger, metrics))
	}
	return app, nil
}

func (a *App) buildController(cfg config.Config, logger *slog.Logger, metrics *telemetry.Metrics, executors ports.ExecutorFactory, tokenIssuer ports.AttemptTokens) error {
	minIsolation := domain.IsolationContainer
	if cfg.Execution.DefaultIsolation != "" {
		if tier := domain.IsolationTier(cfg.Execution.DefaultIsolation); tier.Valid() {
			minIsolation = tier
		}
	}
	policyEngine, err := policy.NewEngine(policy.HardPolicy{
		MinIsolation: minIsolation, RequireNoSubjectNetwork: true,
		RequiredChecks: []string{"compile"}, AllowedLanguages: cfg.Execution.AllowedLanguages,
		AllowedRegions: []string{cfg.Service.Region}, MaxRepositoryBytes: cfg.Limits.MaxRepositoryBytes,
		MaxChangedFiles:      cfg.Limits.MaxChangedFiles,
		ProtectedPaths:       []string{".patchproof/**", ".github/workflows/**"},
		RequireTrustedPolicy: true, DisallowSubmodules: true,
		EnforceTenantFloor: cfg.Execution.EnforceTenantFloor,
	})
	if err != nil {
		return err
	}
	plannerInstance, err := planner.New(cfg.Scheduler.ShardCount, planner.Limits{
		MaxStepCount: cfg.Limits.MaxStepCount, MaxParallelSteps: cfg.Limits.MaxParallelSteps,
		MaxStepWallSeconds: cfg.Limits.MaxRunWallSeconds, MaxRunWallSeconds: cfg.Limits.MaxRunWallSeconds,
		DefaultIsolation: minIsolation, MaxIsolation: domain.IsolationTier(cfg.Execution.MaxIsolation),
		Architecture: runtime.GOARCH,
	})
	if err != nil {
		return err
	}
	signer, err := NewSigner(cfg)
	if err != nil {
		return err
	}
	proofService, err := proof.New(a.Store, a.Artifacts, signer, proof.Config{
		Region: cfg.Service.Region, Version: cfg.Service.Version, RequireAttestation: cfg.Signing.Require,
	}, metrics, logger)
	if err != nil {
		return err
	}
	snapshotter, err := git.New(git.Config{
		WorkDir:               cfg.Execution.LocalWorkDir,
		AllowFileRepositories: cfg.Execution.AllowFileRepositories || cfg.Database.Driver == "memory",
		AllowFullFetch:        cfg.Execution.AllowFullFetch || cfg.Database.Driver == "memory",
		MaxRepositoryBytes:    cfg.Limits.MaxRepositoryBytes,
		MaxFiles:              200000, Timeout: 10 * time.Minute, Logger: logger,
	}, a.Artifacts, nil, logger)
	if err != nil {
		return err
	}
	orchestratorService, err := orchestrator.New(a.Store, plannerInstance, policyEngine, snapshotter, a.Artifacts, proofService, orchestrator.Config{
		Region: cfg.Service.Region, Version: cfg.Service.Version, PollInterval: time.Second,
		ValidationWorkers: 4, MaxRepositoriesBytes: cfg.Limits.MaxRepositoryBytes,
		MaxChangedFiles: cfg.Limits.MaxChangedFiles, MaxRunWallSeconds: cfg.Limits.MaxRunWallSeconds,
		RequireAttestation: cfg.Signing.Require,
		SupportsBrokeredIO: cfg.Execution.BrokerImage != "", ShardCount: cfg.Scheduler.ShardCount, Logger: logger,
	}, metrics)
	if err != nil {
		return err
	}
	placement, err := scheduler.NewPlacement(cfg.Scheduler.Placement)
	if err != nil {
		return err
	}
	schedulerService, err := scheduler.New(a.Store, executors, placement, tokenIssuer, a.Artifacts, scheduler.Config{
		Region: cfg.Service.Region, OwnerID: instanceID(cfg), ShardCount: cfg.Scheduler.ShardCount,
		BatchSize: cfg.Scheduler.BatchSize, LeaseTTL: cfg.Scheduler.LeaseTTL,
		ShardLeaseTTL: 30 * time.Second, PollInterval: cfg.Scheduler.PollInterval,
		MaxStepsPerTenant: cfg.Scheduler.MaxStepsPerTenant, MaxStepsPerCluster: cfg.Scheduler.MaxStepsPerCluster,
		ClusterStaleAfter: cfg.Scheduler.ClusterStaleAfter,
		MaxRunWallSeconds: cfg.Limits.MaxRunWallSeconds, CallbackURL: callbackURL(cfg),
		AttemptTokenTTL:    cfg.Auth.AttemptTokenTTL,
		ActionCacheEnabled: cfg.Scheduler.ActionCacheEnabled, ActionCacheTTL: cfg.Scheduler.ActionCacheTTL,
		CacheEpoch: cacheEpoch(cfg), EngineVersions: a.engines, WorkerImage: cfg.Execution.WorkerImage,
		EngineImage: cfg.Execution.EngineImage, PluginEnvDigest: PluginEnvironmentDigest(cfg),
		Logger: logger,
	}, metrics)
	if err != nil {
		return err
	}
	relay, err := outbox.New(a.Store, a.Bus, outbox.Config{
		Interval: cfg.Events.OutboxInterval, Batch: cfg.Events.OutboxBatch,
		MaxAttempts: cfg.Events.OutboxMaxAttempts,
	}, metrics, logger)
	if err != nil {
		return err
	}
	a.components = append(a.components,
		orchestratorService.Run,
		schedulerService.Run,
		relay.Run,
		func(ctx context.Context) error {
			return a.Bus.Consume(ctx, "controller", []string{
				string(domain.EventStepReady), string(domain.EventRunCreated), string(domain.EventRunPlanned),
			}, func(_ context.Context, _ domain.EventEnvelope) error {
				orchestratorService.Wake()
				return nil
			})
		},
	)
	return nil
}

func (a *App) buildAPI(cfg config.Config, logger *slog.Logger, metrics *telemetry.Metrics, tokenIssuer ports.AttemptTokens) error {
	authenticator, err := NewAuthenticator(cfg)
	if err != nil {
		return err
	}
	runService, err := runservice.New(a.Store, runservice.Config{
		Region: cfg.Service.Region, Limits: runservice.Limits{
			MaxStepCount: cfg.Limits.MaxStepCount, MaxParallelSteps: cfg.Limits.MaxParallelSteps,
			MaxRunWallSeconds: cfg.Limits.MaxRunWallSeconds, DefaultWallSeconds: cfg.Limits.DefaultWallSeconds,
			DefaultCPUSeconds: cfg.Limits.DefaultCPUSeconds, DefaultFuzzCPUSeconds: cfg.Limits.DefaultFuzzCPUSeconds,
			MaxRepositoryBytes: cfg.Limits.MaxRepositoryBytes, MaxChangedFiles: cfg.Limits.MaxChangedFiles,
			APIListMaxLimit: cfg.Limits.APIListMaxLimit,
		}, Logger: logger,
	}, metrics)
	if err != nil {
		return err
	}
	adminService, err := admin.New(a.Store, admin.Config{Region: cfg.Service.Region, Logger: logger})
	if err != nil {
		return err
	}
	attemptService, err := attempts.New(a.Store, nil, cfg.Scheduler.LeaseTTL*2)
	if err != nil {
		return err
	}
	attemptService.ConfigureActionCacheDetails(cfg.Scheduler.ActionCacheEnabled, cfg.Scheduler.ActionCacheTTL, cacheEpoch(cfg), a.engines, cfg.Execution.WorkerImage, cfg.Execution.EngineImage, PluginEnvironmentDigest(cfg))
	reporters := map[string]ports.Reporter{
		"json": jsonreport.New(), "markdown": markdownreport.New(), "sarif": sarifreport.New(),
	}
	limiter := ratelimit.New(cfg.Limits.APIRPSPerTenant, cfg.Limits.APIRPSPerTenant*2)
	limiter.SetResolver(newTenantRateResolver(a.Store, cfg.Limits.APIRPSPerTenant).Resolve)
	server, err := api.New(api.Deps{
		Runs: runService, Admin: adminService, Attempts: attemptService, Tokens: tokenIssuer,
		Authenticator: authenticator, Reporters: reporters, Metrics: metrics, Limiter: limiter,
		Logger: logger, Readiness: a.readiness, PublicURL: cfg.HTTP.ExternalURL,
		Version: cfg.Service.Version, Region: cfg.Service.Region, MaxBodyBytes: cfg.HTTP.MaxBodyBytes,
	})
	if err != nil {
		return err
	}
	a.API = server
	a.Runs = runService
	a.Admin = adminService
	a.Reporters = reporters
	return nil
}

func (a *App) metricsServer(cfg config.Config, logger *slog.Logger, metrics *telemetry.Metrics) func(context.Context) error {
	return func(ctx context.Context) error {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
		server, err := httpserver.New(cfg.Telemetry.MetricsAddr, mux, cfg.HTTP, logger)
		if err != nil {
			return err
		}
		return httpserver.Serve(ctx, server, logger)
	}
}

func (a *App) Run(ctx context.Context) error {
	group := lifecycle.New(ctx, a.Logger)
	for _, component := range a.components {
		group.Go("component", component)
	}
	err := group.Wait()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.Config.HTTP.ShutdownTimeout)
	defer cancel()
	if a.HTTPServer != nil {
		if shutdownErr := a.HTTPServer.Shutdown(shutdownCtx); shutdownErr != nil {
			a.Logger.Warn("http shutdown failed", slog.String("error", shutdownErr.Error()))
		}
	}
	a.Close(shutdownCtx)
	return err
}

func (a *App) Close(_ context.Context) {
	for _, closer := range a.closers {
		if err := closer(); err != nil {
			a.Logger.Warn("close failed", slog.String("error", err.Error()))
		}
	}
	a.closers = nil
}

func (a *App) MetricsHandler() http.Handler {
	if a.Metrics == nil {
		return http.NotFoundHandler()
	}
	return a.Metrics.Handler()
}

func instanceID(cfg config.Config) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "controller"
	}
	sum := sha256.Sum256([]byte(host + cfg.Service.Region + cfg.Service.Name + uuid.NewString()))
	return host + "-" + hex.EncodeToString(sum[:4])
}

func Tolerations(entries []config.Toleration) []corev1.Toleration {
	out := make([]corev1.Toleration, 0, len(entries))
	for _, entry := range entries {
		out = append(out, corev1.Toleration{
			Key: entry.Key, Operator: corev1.TolerationOperator(entry.Operator), Value: entry.Value,
			Effect: corev1.TaintEffect(entry.Effect),
		})
	}
	return out
}

func WorkerEnvironment(cfg config.Config) map[string]string {
	environment := map[string]string{
		"PATCHPROOF_ROLE":           "worker",
		"PATCHPROOF_SERVICE_NAME":   "patchproof-worker",
		"PATCHPROOF_REGION":         cfg.Service.Region,
		"PATCHPROOF_DB_DRIVER":      "memory",
		"PATCHPROOF_EVENTS_DRIVER":  "none",
		"PATCHPROOF_CAS_DRIVER":     cfg.Artifacts.Driver,
		"PATCHPROOF_WORKER_WORKDIR": "/workspace",
		"PATCHPROOF_METRICS_ADDR":   "",
	}
	if cfg.Artifacts.Driver == "fs" {
		environment["PATCHPROOF_CAS_ROOT"] = cfg.Artifacts.Root
	}
	if cfg.Artifacts.Driver == "s3" {
		environment["PATCHPROOF_S3_ENDPOINT"] = cfg.Artifacts.Endpoint
		environment["PATCHPROOF_S3_REGION"] = cfg.Artifacts.Region
		environment["PATCHPROOF_S3_BUCKET"] = cfg.Artifacts.Bucket
		if cfg.Artifacts.ForcePathStyle {
			environment["PATCHPROOF_S3_FORCE_PATH_STYLE"] = "true"
		}
	}
	if len(cfg.Execution.Plugins) > 0 {
		if encoded, err := json.Marshal(cfg.Execution.Plugins); err == nil {
			environment["PATCHPROOF_PLUGINS_JSON"] = string(encoded)
		}
	}
	if cfg.Telemetry.LogLevel != "" {
		environment["PATCHPROOF_LOG_LEVEL"] = cfg.Telemetry.LogLevel
	}
	if cfg.Telemetry.OTLPEndpoint != "" {
		environment["PATCHPROOF_OTLP_ENDPOINT"] = cfg.Telemetry.OTLPEndpoint
	}
	return environment
}

// PluginEnvironmentDigest binds plugin command/env configuration into action
// cache keys. Plugin binaries are covered by the worker image; the configured
// environment and command are deployment inputs that can change behavior.
func PluginEnvironmentDigest(cfg config.Config) string {
	if len(cfg.Execution.Plugins) == 0 {
		return ""
	}
	raw, err := json.Marshal(cfg.Execution.Plugins)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cacheEpoch(cfg config.Config) string {
	if cfg.Scheduler.CacheEpoch != "" {
		return cfg.Scheduler.CacheEpoch
	}
	return cfg.Service.Version
}

func callbackURL(cfg config.Config) string {
	if cfg.Execution.ReportBackURL != "" {
		return cfg.Execution.ReportBackURL
	}
	return strings.TrimRight(cfg.HTTP.ExternalURL, "/")
}
