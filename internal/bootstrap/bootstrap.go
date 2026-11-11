package bootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patchproof/patchproof/engines/goanalyzer"
	"github.com/patchproof/patchproof/engines/gobuild"
	"github.com/patchproof/patchproof/engines/godifferential"
	"github.com/patchproof/patchproof/engines/gofuzz"
	casfs "github.com/patchproof/patchproof/internal/adapters/cas/fs"
	casmemory "github.com/patchproof/patchproof/internal/adapters/cas/memory"
	cass3 "github.com/patchproof/patchproof/internal/adapters/cas/s3"
	"github.com/patchproof/patchproof/internal/adapters/events/memory"
	eventsnats "github.com/patchproof/patchproof/internal/adapters/events/nats"
	"github.com/patchproof/patchproof/internal/adapters/executors/kubernetes"
	"github.com/patchproof/patchproof/internal/adapters/executors/local"
	"github.com/patchproof/patchproof/internal/adapters/executors/remote"
	memorystore "github.com/patchproof/patchproof/internal/adapters/storage/memory"
	postgresstore "github.com/patchproof/patchproof/internal/adapters/storage/postgres"
	"github.com/patchproof/patchproof/internal/app/tokens"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/internal/engine/pluginengine"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/platform/signing"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/internal/security/auth"
	"github.com/patchproof/patchproof/internal/worker"
)

func OpenStore(ctx context.Context, cfg config.Config) (ports.MetadataStore, error) {
	switch cfg.Database.Driver {
	case "postgres":
		store, err := postgresstore.New(ctx, postgresstore.Config{
			DSN: cfg.Database.DSN, MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
			MaxConnLifetime: cfg.Database.MaxConnLifetime, MaxConnIdleTime: cfg.Database.MaxConnIdleTime,
			StatementTimeout: cfg.Database.StatementTimeout, ApplicationName: cfg.Service.Name,
		})
		if err != nil {
			return nil, err
		}
		if cfg.Database.MigrateOnStart {
			if err := store.Migrate(ctx); err != nil {
				store.Close()
				return nil, fmt.Errorf("migrate schema: %w", err)
			}
		}
		return store, nil
	case "memory":
		return memorystore.New(), nil
	default:
		return nil, fmt.Errorf("bootstrap: unsupported metadata driver %q", cfg.Database.Driver)
	}
}

func OpenArtifacts(ctx context.Context, cfg config.Config) (ports.ArtifactStore, error) {
	switch cfg.Artifacts.Driver {
	case "fs":
		return casfs.New(casfs.Config{Root: cfg.Artifacts.Root, CreateRoot: true})
	case "s3":
		return cass3.New(ctx, cass3.Config{
			Endpoint: cfg.Artifacts.Endpoint, Region: cfg.Artifacts.Region, Bucket: cfg.Artifacts.Bucket,
			AccessKeyID: cfg.Artifacts.AccessKeyID, SecretAccessKey: cfg.Artifacts.SecretAccessKey,
			ForcePathStyle: cfg.Artifacts.ForcePathStyle, UsePathStyle: cfg.Artifacts.ForcePathStyle,
			PresignTTL: cfg.Artifacts.PresignTTL, Prefix: cfg.Artifacts.PublicPrefix,
		})
	case "memory":
		return casmemory.New(), nil
	default:
		return nil, fmt.Errorf("bootstrap: unsupported artifact driver %q", cfg.Artifacts.Driver)
	}
}

func OpenBus(ctx context.Context, cfg config.Config) (ports.EventBus, error) {
	switch cfg.Events.Driver {
	case "nats":
		return eventsnats.New(ctx, eventsnats.Config{
			URLs: cfg.Events.URLs, Stream: cfg.Events.Stream, ConsumerPrefix: cfg.Events.ConsumerPrefix,
			MaxAckPending: cfg.Events.MaxAckPending, BatchSize: cfg.Events.BatchSize, AckWait: cfg.Events.AckWait,
			MaxDeliver: 5, ConnectTimeout: 10 * time.Second, Replicas: 1,
		})
	case "memory", "none":
		return memory.New(), nil
	default:
		return nil, fmt.Errorf("bootstrap: unsupported event driver %q", cfg.Events.Driver)
	}
}

func NewEngines(cfg config.Config) (*engine.Registry, error) {
	registry := engine.NewRegistry(goanalyzer.New(), gobuild.New(), gofuzz.New(), godifferential.New())
	reserved := map[string]struct{}{
		"go-analyzer": {}, "go-build": {}, "go-differential": {}, "go-native-fuzz": {},
	}
	for _, pluginConfig := range cfg.Execution.Plugins {
		if _, clash := reserved[pluginConfig.Name]; clash {
			return nil, fmt.Errorf("bootstrap: plugin name %q conflicts with a bundled engine", pluginConfig.Name)
		}
		kinds := make([]domain.StepKind, 0, len(pluginConfig.StepKinds))
		for _, kind := range pluginConfig.StepKinds {
			kinds = append(kinds, domain.StepKind(kind))
		}
		envKeys := make([]string, 0, len(pluginConfig.Env))
		for key := range pluginConfig.Env {
			envKeys = append(envKeys, key)
		}
		sort.Strings(envKeys)
		env := make([]string, 0, len(envKeys))
		for _, key := range envKeys {
			env = append(env, key+"="+pluginConfig.Env[key])
		}
		version := pluginConfig.Version
		if version == "" {
			version = "external"
		}
		external, err := pluginengine.New(pluginengine.Config{
			Name: pluginConfig.Name, Version: version, Command: pluginConfig.Command,
			Env: env, Languages: pluginConfig.Languages, Kinds: kinds,
			MaxFrameBytes: pluginConfig.MaxFrameBytes, Timeout: pluginConfig.Timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("bootstrap: plugin %s: %w", pluginConfig.Name, err)
		}
		registry.Register(external)
	}
	return registry, nil
}

// EngineVersions returns the declared version of every engine reachable by
// this deployment. Workers and controllers must agree on the value because it
// is part of the action cache key.
func EngineVersions(cfg config.Config) (map[string]string, error) {
	registry, err := NewEngines(cfg)
	if err != nil {
		return nil, err
	}
	capabilities := registry.Capabilities()
	versions := make(map[string]string, len(capabilities))
	for _, capability := range capabilities {
		versions[capability.Name] = capability.Version
	}
	return versions, nil
}

func NewWorker(cfg config.Config, store ports.ArtifactStore, engines *engine.Registry, logger *slog.Logger) (*worker.Runner, error) {
	return worker.New(worker.Config{
		WorkDir: cfg.Worker.WorkDir, MaxExtractBytes: cfg.Limits.MaxRepositoryBytes,
		Clock: nil, Logger: logger,
	}, worker.Deps{Engines: engines, Store: store})
}

func NewTokenIssuer(cfg config.Config) (ports.AttemptTokens, error) {
	key := []byte(cfg.Auth.AttemptTokenKey)
	if len(key) < 32 {
		if cfg.Auth.Mode != "dev" || cfg.Auth.DevToken == "" {
			return nil, fmt.Errorf("bootstrap: auth.attemptTokenKey must be at least 32 characters")
		}
		sum := sha256.Sum256([]byte("patchproof-attempt-token:" + cfg.Auth.DevToken))
		key = sum[:]
	}
	return tokens.NewIssuer(key)
}

func NewAuthenticator(cfg config.Config) (auth.Authenticator, error) {
	switch cfg.Auth.Mode {
	case "dev":
		tenantID, err := domain.ParseTenantID(cfg.Auth.DevTenantID)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
		subject := cfg.Auth.DevSubject
		if subject == "" {
			subject = "patchproof-dev"
		}
		roles := cfg.Auth.DevRoles
		if len(roles) == 0 {
			roles = []string{"admin", "operator", "viewer", "platform-admin"}
		}
		return &auth.DevAuthenticator{Token: cfg.Auth.DevToken, TenantID: tenantID, Subject: subject, Roles: roles}, nil
	case "oidc":
		return auth.NewOIDC(auth.OIDCConfig{
			Issuer: cfg.Auth.Issuer, Audience: cfg.Auth.Audience, JWKSURL: cfg.Auth.JWKSURL,
			TenantClaim: cfg.Auth.TenantClaim, RolesClaim: cfg.Auth.RolesClaim, GroupsClaim: cfg.Auth.GroupsClaim,
			ClockSkew: cfg.Auth.ClockSkew, CacheTTL: cfg.Auth.JWKSRefresh,
		})
	default:
		return nil, fmt.Errorf("bootstrap: unsupported auth mode %q", cfg.Auth.Mode)
	}
}

func NewSigner(cfg config.Config) (ports.Signer, error) {
	keyID := cfg.Signing.KeyID
	if keyID == "" {
		keyID = "patchproof-default"
	}
	path := cfg.Signing.PrivateKeyFile
	if path == "" {
		if cfg.Database.Driver != "memory" {
			if cfg.Signing.Require {
				return nil, fmt.Errorf("bootstrap: signing key is required")
			}
			return nil, nil
		}
		path = filepath.Join(".patchproof", "keys", "signing.pem")
	}
	var signer *signing.Ed25519Signer
	var err error
	if cfg.Database.Driver == "memory" {
		signer, err = signing.LoadOrGenerate(path, keyID)
	} else {
		var key ed25519.PrivateKey
		key, err = signing.LoadPrivateKey(path)
		if err == nil {
			signer, err = signing.NewEd25519Signer(keyID, key)
		}
	}
	if err != nil {
		return nil, err
	}
	publicPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".pub"
	if _, err := os.Stat(publicPath); errors.Is(err, os.ErrNotExist) {
		encoded, err := signing.EncodePublicKeyPEM(signer.PublicKey())
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(publicPath, encoded, 0o644); err != nil {
			return nil, fmt.Errorf("write public key: %w", err)
		}
	}
	return signer, nil
}

type ExecutorFactory struct {
	config config.Config
	runner ports.StepRunner
	logger *slog.Logger
	mu     sync.Mutex
	local  *local.Executor
	k8s    *kubernetes.Executor
	remote map[domain.ClusterID]*remote.Executor
}

func NewExecutorFactory(cfg config.Config, runner ports.StepRunner, logger *slog.Logger) (*ExecutorFactory, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return &ExecutorFactory{config: cfg, runner: runner, logger: logger, remote: map[domain.ClusterID]*remote.Executor{}}, nil
}

func (f *ExecutorFactory) ForCluster(cluster domain.Cluster) (ports.Executor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cluster.ExecutorKind {
	case "local":
		if f.runner == nil {
			return nil, fmt.Errorf("bootstrap: local executor requires an in-process step runner")
		}
		if f.local == nil {
			isolation := domain.IsolationTrustedLocal
			executor, err := local.New(local.Config{
				Name: "local", Region: f.config.Service.Region, MaxConcurrent: f.config.Execution.MaxConcurrent,
				WorkDir: f.config.Execution.LocalWorkDir, AllowedIsolation: isolation,
				ActiveDeadline: f.config.Execution.ActiveDeadline,
			}, f.runner)
			if err != nil {
				return nil, err
			}
			f.local = executor
		}
		return f.local, nil
	case "kubernetes":
		if f.k8s == nil {
			namespace := cluster.Namespace
			if namespace == "" {
				namespace = f.config.Execution.Namespace
			}
			callback := f.config.Execution.ReportBackURL
			if callback == "" {
				callback = strings.TrimRight(f.config.HTTP.ExternalURL, "/")
			}
			executor, err := kubernetes.New(kubernetes.Config{
				ClusterID: cluster.ID, Region: f.config.Execution.ClusterRegion, Namespace: namespace,
				Kubeconfig: f.config.Execution.Kubeconfig, ServiceAccount: f.config.Execution.ServiceAccount,
				WorkerImage: f.config.Execution.WorkerImage, EngineImage: f.config.Execution.EngineImage,
				BrokerImage: f.config.Execution.BrokerImage, BrokerServiceAccount: f.config.Execution.BrokerServiceAccount,
				BrokeredStorageClass: f.config.Execution.BrokeredStorageClass, WorkspaceSizeGiB: f.config.Execution.WorkspaceSizeGiB,
				RuntimeClass: f.config.Execution.RuntimeClass, JobTTL: f.config.Execution.JobTTL,
				ActiveDeadline: f.config.Execution.ActiveDeadline, MaxConcurrent: f.config.Execution.MaxConcurrent,
				CallbackURL: callback, ExtraEnv: WorkerEnvironment(f.config),
				NodeSelector:                 f.config.Execution.NodeSelector,
				Tolerations:                  Tolerations(f.config.Execution.Tolerations),
				AutomountServiceAccountToken: f.config.Execution.AutomountWorkerToken,
				Languages:                    ConfiguredLanguages(f.config), Engines: ConfiguredEngines(f.config),
			})
			if err != nil {
				return nil, err
			}
			f.k8s = executor
		}
		return f.k8s, nil
	case "remote":
		executor, ok := f.remote[cluster.ID]
		if !ok {
			created, err := remote.New(remote.Config{
				Endpoint: cluster.Endpoint, Token: f.config.Execution.AgentToken,
				ClusterID: cluster.ID, Timeout: 30 * time.Second,
			})
			if err != nil {
				return nil, err
			}
			f.remote[cluster.ID] = created
			executor = created
		}
		return executor, nil
	default:
		return nil, fmt.Errorf("bootstrap: unknown executor kind %q for cluster %s", cluster.ExecutorKind, cluster.ID)
	}
}

func EnsureDevTenant(ctx context.Context, store ports.MetadataStore, cfg config.Config) error {
	tenantID, err := domain.ParseTenantID(cfg.Auth.DevTenantID)
	if err != nil {
		return fmt.Errorf("bootstrap: invalid dev tenant: %w", err)
	}
	if _, err := store.GetTenant(ctx, tenantID); err == nil {
		return nil
	} else if !domain.IsNotFound(err) {
		return err
	}
	now := time.Now().UTC()
	tenant := domain.Tenant{
		ID: tenantID, HomeRegion: cfg.Service.Region, SecurityTier: domain.SecurityTierStandard,
		Status: domain.TenantActive, ActiveWriterEpoch: 1, DisplayName: "development",
		Quota: domain.TenantQuota{
			MaxConcurrentRuns: 32, MaxConcurrentSteps: 128, CPUCoreSecondsDay: 500000,
			FuzzSecondsDay: 100000, StorageBytes: 50 << 30, APIRPS: cfg.Limits.APIRPSPerTenant,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := tenant.Validate(); err != nil {
		return err
	}
	return store.CreateTenant(ctx, tenant)
}

func ConfiguredLanguages(cfg config.Config) []string {
	if len(cfg.Execution.AllowedLanguages) > 0 {
		return append([]string(nil), cfg.Execution.AllowedLanguages...)
	}
	return []string{"go"}
}

func ConfiguredEngines(cfg config.Config) []string {
	engines := []string{"go-analyzer", "go-build", "go-differential", "go-native-fuzz"}
	for _, plugin := range cfg.Execution.Plugins {
		engines = append(engines, plugin.Name)
	}
	return engines
}

func EnsureLocalCluster(ctx context.Context, store ports.MetadataStore, cfg config.Config, now time.Time) (domain.Cluster, error) {
	clusters, err := store.ListClusters(ctx, "")
	if err != nil {
		return domain.Cluster{}, err
	}
	for _, cluster := range clusters {
		if cluster.ID != "local" {
			continue
		}
		cluster.LastHeartbeat = now
		cluster.Healthy = true
		if err := store.UpsertCluster(ctx, cluster); err != nil {
			return domain.Cluster{}, err
		}
		return cluster, nil
	}
	cluster := domain.Cluster{
		ID: "local", Region: cfg.Service.Region, ExecutorKind: "local",
		Architectures: []string{runtime.GOARCH}, Languages: []string{"go"},
		Engines:           []string{"go-analyzer", "go-build", "go-differential", "go-native-fuzz"},
		IsolationTiers:    []domain.IsolationTier{domain.IsolationTrustedLocal},
		AvailableCPUMilli: 64_000, AvailableMemoryMiB: 262_144, MaxConcurrentSteps: cfg.Execution.MaxConcurrent,
		CostTier: 0, CasRegion: cfg.Service.Region, Healthy: true, Version: cfg.Service.Version,
		LastHeartbeat: now, Labels: map[string]string{"features": "in-process"},
	}
	if err := store.UpsertCluster(ctx, cluster); err != nil {
		return domain.Cluster{}, err
	}
	return cluster, nil
}

// LocalClusterHeartbeat keeps the in-process executor visible to placement.
func LocalClusterHeartbeat(store ports.MetadataStore, cfg config.Config, logger *slog.Logger) func(context.Context) error {
	interval := cfg.Worker.Heartbeat
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if stale := cfg.Scheduler.ClusterStaleAfter; stale > 0 && interval > stale/3 {
		interval = stale / 3
	}
	if interval < time.Second {
		interval = time.Second
	}
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if _, err := EnsureLocalCluster(ctx, store, cfg, time.Now().UTC()); err != nil && ctx.Err() == nil {
				logger.Warn("local cluster heartbeat failed", slog.String("error", err.Error()))
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}
}

func Readiness(store ports.MetadataStore, artifacts ports.ArtifactStore, bus ports.EventBus) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := store.Ping(ctx); err != nil {
			return fmt.Errorf("metadata store: %w", err)
		}
		if err := artifacts.Health(ctx); err != nil {
			return fmt.Errorf("artifact store: %w", err)
		}
		if bus != nil {
			if err := bus.Health(ctx); err != nil {
				return fmt.Errorf("event bus: %w", err)
			}
		}
		return nil
	}
}

func Region(run domain.Run, fallback string) string {
	if run.Region != "" {
		return run.Region
	}
	return fallback
}
