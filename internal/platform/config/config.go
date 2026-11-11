package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"gopkg.in/yaml.v3"
)

type Role string

const (
	RoleAPI          Role = "api"
	RoleController   Role = "controller"
	RoleWorker       Role = "worker"
	RoleClusterAgent Role = "cluster-agent"
	RoleAllInOne     Role = "all-in-one"
	RoleProof        Role = "proof"
)

func (r Role) Valid() bool {
	switch r {
	case RoleAPI, RoleController, RoleWorker, RoleClusterAgent, RoleAllInOne, RoleProof:
		return true
	}
	return false
}

type Config struct {
	Service   ServiceConfig   `yaml:"service"`
	HTTP      HTTPConfig      `yaml:"http"`
	Database  DatabaseConfig  `yaml:"database"`
	Events    EventsConfig    `yaml:"events"`
	Artifacts ArtifactsConfig `yaml:"artifacts"`
	Auth      AuthConfig      `yaml:"auth"`
	Execution ExecutionConfig `yaml:"execution"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Worker    WorkerConfig    `yaml:"worker"`
	Signing   SigningConfig   `yaml:"signing"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
	Limits    LimitsConfig    `yaml:"limits"`
	State     StateConfig     `yaml:"state"`
}

type ServiceConfig struct {
	Role    Role   `yaml:"role"`
	Name    string `yaml:"name"`
	Region  string `yaml:"region"`
	Version string `yaml:"version"`
	Cluster string `yaml:"cluster"`
}

type HTTPConfig struct {
	ListenAddr        string        `yaml:"listenAddr"`
	ExternalURL       string        `yaml:"externalUrl"`
	ReadHeaderTimeout time.Duration `yaml:"readHeaderTimeout"`
	ReadTimeout       time.Duration `yaml:"readTimeout"`
	WriteTimeout      time.Duration `yaml:"writeTimeout"`
	IdleTimeout       time.Duration `yaml:"idleTimeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdownTimeout"`
	MaxBodyBytes      int64         `yaml:"maxBodyBytes"`
	TLSCertFile       string        `yaml:"tlsCertFile"`
	TLSKeyFile        string        `yaml:"tlsKeyFile"`
	TrustedProxies    []string      `yaml:"trustedProxies"`
}

type DatabaseConfig struct {
	Driver           string        `yaml:"driver"`
	DSN              string        `yaml:"dsn"`
	MaxConns         int32         `yaml:"maxConns"`
	MinConns         int32         `yaml:"minConns"`
	MaxConnLifetime  time.Duration `yaml:"maxConnLifetime"`
	MaxConnIdleTime  time.Duration `yaml:"maxConnIdleTime"`
	StatementTimeout time.Duration `yaml:"statementTimeout"`
	MigrateOnStart   bool          `yaml:"migrateOnStart"`
	SQLitePath       string        `yaml:"sqlitePath"`
}

type EventsConfig struct {
	Driver         string        `yaml:"driver"`
	URLs           []string      `yaml:"urls"`
	Stream         string        `yaml:"stream"`
	ConsumerPrefix string        `yaml:"consumerPrefix"`
	MaxAckPending  int           `yaml:"maxAckPending"`
	BatchSize      int           `yaml:"batchSize"`
	AckWait        time.Duration `yaml:"ackWait"`
	OutboxInterval time.Duration `yaml:"outboxInterval"`
	OutboxBatch    int           `yaml:"outboxBatch"`
}

type ArtifactsConfig struct {
	Driver          string        `yaml:"driver"`
	Root            string        `yaml:"root"`
	Endpoint        string        `yaml:"endpoint"`
	Region          string        `yaml:"region"`
	Bucket          string        `yaml:"bucket"`
	AccessKeyID     string        `yaml:"accessKeyId"`
	SecretAccessKey string        `yaml:"secretAccessKey"`
	ForcePathStyle  bool          `yaml:"forcePathStyle"`
	PublicPrefix    string        `yaml:"publicPrefix"`
	PresignTTL      time.Duration `yaml:"presignTtl"`
}

type AuthConfig struct {
	Mode            string        `yaml:"mode"`
	DevToken        string        `yaml:"devToken"`
	Issuer          string        `yaml:"issuer"`
	Audience        string        `yaml:"audience"`
	JWKSURL         string        `yaml:"jwksUrl"`
	TenantClaim     string        `yaml:"tenantClaim"`
	RolesClaim      string        `yaml:"rolesClaim"`
	GroupsClaim     string        `yaml:"groupsClaim"`
	JWKSRefresh     time.Duration `yaml:"jwksRefresh"`
	ClockSkew       time.Duration `yaml:"clockSkew"`
	AttemptTokenKey string        `yaml:"attemptTokenKey"`
	AttemptTokenTTL time.Duration `yaml:"attemptTokenTtl"`
	AdminSubjects   []string      `yaml:"adminSubjects"`
	InternalToken   string        `yaml:"internalToken"`
	DevTenantID     string        `yaml:"devTenantId"`
	DevSubject      string        `yaml:"devSubject"`
	DevRoles        []string      `yaml:"devRoles"`
}

type ExecutionConfig struct {
	DefaultIsolation        string            `yaml:"defaultIsolation"`
	MaxIsolation            string            `yaml:"maxIsolation"`
	AllowedLanguages        []string          `yaml:"allowedLanguages"`
	RuntimeClass            string            `yaml:"runtimeClass"`
	WorkerImage             string            `yaml:"workerImage"`
	EngineImage             string            `yaml:"engineImage"`
	Namespace               string            `yaml:"namespace"`
	Kubeconfig              string            `yaml:"kubeconfig"`
	ServiceAccount          string            `yaml:"serviceAccount"`
	JobTTL                  time.Duration     `yaml:"jobTtl"`
	JobBackoffLimit         int32             `yaml:"jobBackoffLimit"`
	ActiveDeadline          time.Duration     `yaml:"activeDeadline"`
	LocalRunner             bool              `yaml:"localRunner"`
	LocalWorkDir            string            `yaml:"localWorkDir"`
	RequireSignedImages     bool              `yaml:"requireSignedImages"`
	MaxConcurrent           int               `yaml:"maxConcurrent"`
	AgentEndpoint           string            `yaml:"agentEndpoint"`
	AgentToken              string            `yaml:"agentToken"`
	ClusterID               string            `yaml:"clusterId"`
	ClusterRegion           string            `yaml:"clusterRegion"`
	ReportBackURL           string            `yaml:"reportBackUrl"`
	SupportedIsolationTiers []string          `yaml:"supportedIsolationTiers,omitempty"`
	NodeSelector            map[string]string `yaml:"nodeSelector,omitempty"`
	Tolerations             []Toleration      `yaml:"tolerations,omitempty"`
	AllowFileRepositories   bool              `yaml:"allowFileRepositories"`
	AllowFullFetch          bool              `yaml:"allowFullFetch"`
	EnforceTenantFloor      bool              `yaml:"enforceTenantFloor"`
	AutomountWorkerToken    bool              `yaml:"automountWorkerToken"`
	BrokerImage             string            `yaml:"brokerImage"`
	BrokerServiceAccount    string            `yaml:"brokerServiceAccount"`
	BrokeredStorageClass    string            `yaml:"brokeredStorageClass"`
	WorkspaceSizeGiB        int64             `yaml:"workspaceSizeGiB"`
	Plugins                 []PluginConfig    `yaml:"plugins,omitempty"`
}

type PluginConfig struct {
	Name          string            `yaml:"name" json:"name"`
	Version       string            `yaml:"version,omitempty" json:"version,omitempty"`
	Command       []string          `yaml:"command" json:"command"`
	Env           map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Languages     []string          `yaml:"languages" json:"languages"`
	StepKinds     []string          `yaml:"stepKinds" json:"stepKinds"`
	MaxFrameBytes int               `yaml:"maxFrameBytes,omitempty" json:"maxFrameBytes,omitempty"`
	Timeout       time.Duration     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type Toleration struct {
	Key      string `yaml:"key" json:"key"`
	Operator string `yaml:"operator,omitempty" json:"operator,omitempty"`
	Value    string `yaml:"value,omitempty" json:"value,omitempty"`
	Effect   string `yaml:"effect,omitempty" json:"effect,omitempty"`
}

type SchedulerConfig struct {
	ShardCount         int           `yaml:"shardCount"`
	LeaseTTL           time.Duration `yaml:"leaseTtl"`
	RenewInterval      time.Duration `yaml:"renewInterval"`
	PollInterval       time.Duration `yaml:"pollInterval"`
	BatchSize          int           `yaml:"batchSize"`
	MaxStepsPerTenant  int           `yaml:"maxStepsPerTenant"`
	MaxStepsPerCluster int           `yaml:"maxStepsPerCluster"`
	ClusterStaleAfter  time.Duration `yaml:"clusterStaleAfter"`
	Placement          string        `yaml:"placement"`
	PriorityBoostMax   int           `yaml:"priorityBoostMax"`
	ActionCacheEnabled bool          `yaml:"actionCacheEnabled"`
	ActionCacheTTL     time.Duration `yaml:"actionCacheTtl"`
	CacheEpoch         string        `yaml:"cacheEpoch"`
}

type WorkerConfig struct {
	WorkDir          string        `yaml:"workDir"`
	ControllerURL    string        `yaml:"controllerUrl"`
	StepSpecFile     string        `yaml:"stepSpecFile"`
	Heartbeat        time.Duration `yaml:"heartbeat"`
	UploadRetries    int           `yaml:"uploadRetries"`
	MaxOutputBytes   int64         `yaml:"maxOutputBytes"`
	OfflineOutputDir string        `yaml:"offlineOutputDir"`
}

type SigningConfig struct {
	Algorithm      string `yaml:"algorithm"`
	KeyID          string `yaml:"keyId"`
	PrivateKeyFile string `yaml:"privateKeyFile"`
	PublicKeyFile  string `yaml:"publicKeyFile"`
	Require        bool   `yaml:"require"`
	KMSKeyURI      string `yaml:"kmsKeyUri"`
}

type TelemetryConfig struct {
	LogLevel         string  `yaml:"logLevel"`
	LogFormat        string  `yaml:"logFormat"`
	MetricsAddr      string  `yaml:"metricsAddr"`
	OTLPEndpoint     string  `yaml:"otlpEndpoint"`
	OTLPInsecure     bool    `yaml:"otlpInsecure"`
	SampleRatio      float64 `yaml:"sampleRatio"`
	ServiceNamespace string  `yaml:"serviceNamespace"`
}

type LimitsConfig struct {
	MaxRepositoryBytes      int64   `yaml:"maxRepositoryBytes"`
	MaxChangedFiles         int     `yaml:"maxChangedFiles"`
	MaxChangedFileBytes     int64   `yaml:"maxChangedFileBytes"`
	MaxStepCount            int     `yaml:"maxStepCount"`
	MaxRunWallSeconds       int     `yaml:"maxRunWallSeconds"`
	DefaultWallSeconds      int     `yaml:"defaultWallSeconds"`
	DefaultCPUSeconds       float64 `yaml:"defaultCpuSeconds"`
	DefaultFuzzCPUSeconds   float64 `yaml:"defaultFuzzCpuSeconds"`
	DefaultMaxParallelSteps int     `yaml:"defaultMaxParallelSteps"`
	MaxParallelSteps        int     `yaml:"maxParallelSteps"`
	APIRPSPerTenant         int     `yaml:"apiRpsPerTenant"`
	APIListMaxLimit         int     `yaml:"apiListMaxLimit"`
}

type StateConfig struct {
	Driver string `yaml:"driver"`
}

func Default() Config {
	return Config{
		Service: ServiceConfig{Name: "patchproof", Region: "region-a", Version: "dev"},
		HTTP: HTTPConfig{
			ListenAddr:        ":8080",
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			ShutdownTimeout:   20 * time.Second,
			MaxBodyBytes:      2 << 20,
		},
		Database: DatabaseConfig{
			Driver:           "postgres",
			MaxConns:         20,
			MinConns:         2,
			MaxConnLifetime:  time.Hour,
			MaxConnIdleTime:  15 * time.Minute,
			StatementTimeout: 30 * time.Second,
		},
		Events: EventsConfig{
			Driver:         "nats",
			Stream:         "PATCHPROOF",
			ConsumerPrefix: "patchproof",
			MaxAckPending:  512,
			BatchSize:      64,
			AckWait:        30 * time.Second,
			OutboxInterval: 500 * time.Millisecond,
			OutboxBatch:    128,
		},
		Artifacts: ArtifactsConfig{
			Driver:     "fs",
			Root:       ".patchproof/cas",
			PresignTTL: 10 * time.Minute,
		},
		Auth: AuthConfig{
			Mode:            "dev",
			TenantClaim:     "tenant_id",
			RolesClaim:      "roles",
			GroupsClaim:     "groups",
			JWKSRefresh:     10 * time.Minute,
			ClockSkew:       60 * time.Second,
			AttemptTokenTTL: 30 * time.Minute,
		},
		Execution: ExecutionConfig{
			DefaultIsolation:   "trusted-local",
			MaxIsolation:       "untrusted-gvisor",
			AllowedLanguages:   []string{"go"},
			Namespace:          "patchproof-execution",
			ServiceAccount:     "patchproof-worker",
			JobTTL:             30 * time.Minute,
			ActiveDeadline:     20 * time.Minute,
			LocalWorkDir:       os.TempDir(),
			MaxConcurrent:      64,
			EnforceTenantFloor: true,
		},
		Scheduler: SchedulerConfig{
			ShardCount:         16,
			LeaseTTL:           2 * time.Minute,
			RenewInterval:      30 * time.Second,
			PollInterval:       time.Second,
			BatchSize:          64,
			MaxStepsPerTenant:  64,
			MaxStepsPerCluster: 256,
			ClusterStaleAfter:  90 * time.Second,
			Placement:          "score",
			ActionCacheEnabled: true,
			ActionCacheTTL:     168 * time.Hour,
		},
		Worker: WorkerConfig{
			WorkDir:        "/workspace",
			Heartbeat:      15 * time.Second,
			UploadRetries:  4,
			MaxOutputBytes: 1 << 30,
		},
		Signing: SigningConfig{Algorithm: "ed25519"},
		Telemetry: TelemetryConfig{
			LogLevel:         "info",
			LogFormat:        "json",
			MetricsAddr:      ":9090",
			SampleRatio:      0.1,
			ServiceNamespace: "patchproof",
		},
		Limits: LimitsConfig{
			MaxRepositoryBytes:      2 << 30,
			MaxChangedFiles:         2000,
			MaxChangedFileBytes:     64 << 20,
			MaxStepCount:            64,
			MaxRunWallSeconds:       3600,
			DefaultWallSeconds:      900,
			DefaultCPUSeconds:       1200,
			DefaultFuzzCPUSeconds:   600,
			DefaultMaxParallelSteps: 16,
			MaxParallelSteps:        64,
			APIRPSPerTenant:         20,
			APIListMaxLimit:         200,
		},
		State: StateConfig{Driver: "postgres"},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("decode config %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) error {
	stringsMap := []struct {
		key   string
		value *string
	}{
		{"PATCHPROOF_ROLE", (*string)(&cfg.Service.Role)},
		{"PATCHPROOF_REGION", &cfg.Service.Region},
		{"PATCHPROOF_SERVICE_NAME", &cfg.Service.Name},
		{"PATCHPROOF_VERSION", &cfg.Service.Version},
		{"PATCHPROOF_CLUSTER", &cfg.Service.Cluster},
		{"PATCHPROOF_HTTP_ADDR", &cfg.HTTP.ListenAddr},
		{"PATCHPROOF_EXTERNAL_URL", &cfg.HTTP.ExternalURL},
		{"PATCHPROOF_DB_DRIVER", &cfg.Database.Driver},
		{"PATCHPROOF_POSTGRES_DSN", &cfg.Database.DSN},
		{"PATCHPROOF_SQLITE_PATH", &cfg.Database.SQLitePath},
		{"PATCHPROOF_EVENTS_DRIVER", &cfg.Events.Driver},
		{"PATCHPROOF_CAS_DRIVER", &cfg.Artifacts.Driver},
		{"PATCHPROOF_CAS_ROOT", &cfg.Artifacts.Root},
		{"PATCHPROOF_S3_ENDPOINT", &cfg.Artifacts.Endpoint},
		{"PATCHPROOF_S3_REGION", &cfg.Artifacts.Region},
		{"PATCHPROOF_S3_BUCKET", &cfg.Artifacts.Bucket},
		{"PATCHPROOF_S3_ACCESS_KEY", &cfg.Artifacts.AccessKeyID},
		{"PATCHPROOF_S3_SECRET_KEY", &cfg.Artifacts.SecretAccessKey},
		{"PATCHPROOF_RUNTIME_CLASS", &cfg.Execution.RuntimeClass},
		{"PATCHPROOF_AUTH_MODE", &cfg.Auth.Mode},
		{"PATCHPROOF_DEV_TOKEN", &cfg.Auth.DevToken},
		{"PATCHPROOF_DEV_TENANT_ID", &cfg.Auth.DevTenantID},
		{"PATCHPROOF_DEV_SUBJECT", &cfg.Auth.DevSubject},
		{"PATCHPROOF_OIDC_ISSUER", &cfg.Auth.Issuer},
		{"PATCHPROOF_OIDC_AUDIENCE", &cfg.Auth.Audience},
		{"PATCHPROOF_OIDC_JWKS_URL", &cfg.Auth.JWKSURL},
		{"PATCHPROOF_ATTEMPT_TOKEN_KEY", &cfg.Auth.AttemptTokenKey},
		{"PATCHPROOF_INTERNAL_TOKEN", &cfg.Auth.InternalToken},
		{"PATCHPROOF_WORKER_IMAGE", &cfg.Execution.WorkerImage},
		{"PATCHPROOF_ENGINE_IMAGE", &cfg.Execution.EngineImage},
		{"PATCHPROOF_KUBECONFIG", &cfg.Execution.Kubeconfig},
		{"PATCHPROOF_EXECUTION_NAMESPACE", &cfg.Execution.Namespace},
		{"PATCHPROOF_CLUSTER_ID", &cfg.Execution.ClusterID},
		{"PATCHPROOF_BROKER_IMAGE", &cfg.Execution.BrokerImage},
		{"PATCHPROOF_BROKER_SERVICE_ACCOUNT", &cfg.Execution.BrokerServiceAccount},
		{"PATCHPROOF_BROKERED_STORAGE_CLASS", &cfg.Execution.BrokeredStorageClass},
		{"PATCHPROOF_CACHE_EPOCH", &cfg.Scheduler.CacheEpoch},
		{"PATCHPROOF_CLUSTER_REGION", &cfg.Execution.ClusterRegion},
		{"PATCHPROOF_AGENT_ENDPOINT", &cfg.Execution.AgentEndpoint},
		{"PATCHPROOF_AGENT_TOKEN", &cfg.Execution.AgentToken},
		{"PATCHPROOF_WORKER_WORKDIR", &cfg.Worker.WorkDir},
		{"PATCHPROOF_WORKER_OFFLINE_OUTPUT_DIR", &cfg.Worker.OfflineOutputDir},
		{"PATCHPROOF_SIGNING_KEY", &cfg.Signing.PrivateKeyFile},
		{"PATCHPROOF_SIGNING_KEY_ID", &cfg.Signing.KeyID},
		{"PATCHPROOF_LOG_LEVEL", &cfg.Telemetry.LogLevel},
		{"PATCHPROOF_LOG_FORMAT", &cfg.Telemetry.LogFormat},
		{"PATCHPROOF_METRICS_ADDR", &cfg.Telemetry.MetricsAddr},
		{"PATCHPROOF_OTLP_ENDPOINT", &cfg.Telemetry.OTLPEndpoint},
	}
	for _, item := range stringsMap {
		if value, ok := os.LookupEnv(item.key); ok {
			*item.value = value
		}
	}
	boolsMap := []struct {
		key   string
		value *bool
	}{
		{"PATCHPROOF_DB_MIGRATE_ON_START", &cfg.Database.MigrateOnStart},
		{"PATCHPROOF_S3_FORCE_PATH_STYLE", &cfg.Artifacts.ForcePathStyle},
		{"PATCHPROOF_ENFORCE_TENANT_FLOOR", &cfg.Execution.EnforceTenantFloor},
		{"PATCHPROOF_ALLOW_FILE_REPOSITORIES", &cfg.Execution.AllowFileRepositories},
		{"PATCHPROOF_ALLOW_FULL_FETCH", &cfg.Execution.AllowFullFetch},
		{"PATCHPROOF_AUTOMOUNT_WORKER_TOKEN", &cfg.Execution.AutomountWorkerToken},
		{"PATCHPROOF_ACTION_CACHE_ENABLED", &cfg.Scheduler.ActionCacheEnabled},
		{"PATCHPROOF_SIGNING_REQUIRE", &cfg.Signing.Require},
	}
	for _, item := range boolsMap {
		if value, ok := os.LookupEnv(item.key); ok {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%s: %w", item.key, err)
			}
			*item.value = parsed
		}
	}
	if raw, ok := os.LookupEnv("PATCHPROOF_PLUGINS_JSON"); ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Execution.Plugins); err != nil {
			return fmt.Errorf("PATCHPROOF_PLUGINS_JSON: %w", err)
		}
	}
	if raw, ok := os.LookupEnv("PATCHPROOF_NATS_URL"); ok {
		cfg.Events.URLs = strings.Split(raw, ",")
	}
	if raw, ok := os.LookupEnv("PATCHPROOF_NATS_URLS"); ok {
		cfg.Events.URLs = strings.Split(raw, ",")
	}
	intsMap := []struct {
		key   string
		value *int
	}{
		{"PATCHPROOF_SHARD_COUNT", &cfg.Scheduler.ShardCount},
		{"PATCHPROOF_SCHEDULER_BATCH", &cfg.Scheduler.BatchSize},
		{"PATCHPROOF_MAX_PARALLEL_STEPS", &cfg.Limits.MaxParallelSteps},
	}
	for _, item := range intsMap {
		if value, ok := os.LookupEnv(item.key); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("%s: %w", item.key, err)
			}
			*item.value = parsed
		}
	}
	if value, ok := os.LookupEnv("PATCHPROOF_ACTION_CACHE_TTL"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("PATCHPROOF_ACTION_CACHE_TTL: %w", err)
		}
		cfg.Scheduler.ActionCacheTTL = parsed
	}
	if value, ok := os.LookupEnv("PATCHPROOF_LEASE_TTL"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("PATCHPROOF_LEASE_TTL: %w", err)
		}
		cfg.Scheduler.LeaseTTL = parsed
	}
	return nil
}

func (c Config) Validate() error {
	if c.Service.Role != "" && !c.Service.Role.Valid() {
		return fmt.Errorf("service.role: unknown role %q", c.Service.Role)
	}
	if c.Service.Region == "" {
		return fmt.Errorf("service.region: must not be empty")
	}
	if c.Service.Name == "" {
		return fmt.Errorf("service.name: must not be empty")
	}
	if c.HTTP.ListenAddr == "" {
		return fmt.Errorf("http.listenAddr: must not be empty")
	}
	if c.HTTP.ExternalURL != "" {
		u, err := url.Parse(c.HTTP.ExternalURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("http.externalUrl: must be an absolute URL")
		}
	}
	if c.Service.Role != RoleWorker {
		switch c.Database.Driver {
		case "postgres":
			if c.Database.DSN == "" {
				return fmt.Errorf("database.dsn: required for postgres")
			}
		case "sqlite":
			if c.Database.SQLitePath == "" {
				return fmt.Errorf("database.sqlitePath: required for sqlite")
			}
		case "memory":
		default:
			return fmt.Errorf("database.driver: unsupported driver %q", c.Database.Driver)
		}
	}
	if c.Database.MaxConns <= 0 || c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		return fmt.Errorf("database: invalid connection pool bounds")
	}
	if c.Service.Role != RoleWorker {
		switch c.Events.Driver {
		case "nats":
			if len(c.Events.URLs) == 0 || c.Events.URLs[0] == "" {
				return fmt.Errorf("events.urls: required for nats")
			}
		case "memory", "none":
		default:
			return fmt.Errorf("events.driver: unsupported driver %q", c.Events.Driver)
		}
	}
	switch c.Artifacts.Driver {
	case "fs":
		if c.Artifacts.Root == "" {
			return fmt.Errorf("artifacts.root: required for fs")
		}
	case "s3":
		if c.Artifacts.Bucket == "" {
			return fmt.Errorf("artifacts.bucket: required for s3")
		}
	case "memory":
	default:
		return fmt.Errorf("artifacts.driver: unsupported driver %q", c.Artifacts.Driver)
	}
	switch c.Auth.Mode {
	case "dev":
		if c.Service.Role == RoleAPI || c.Service.Role == RoleAllInOne {
			if c.Auth.DevToken == "" {
				return fmt.Errorf("auth.devToken: required in dev mode")
			}
			if _, err := domain.ParseTenantID(c.Auth.DevTenantID); err != nil {
				return fmt.Errorf("auth.devTenantId: a valid tenant UUID is required in dev mode")
			}
		}
	case "oidc":
		if c.Auth.Issuer == "" || c.Auth.Audience == "" || c.Auth.JWKSURL == "" {
			return fmt.Errorf("auth: issuer, audience and jwksUrl are required for oidc")
		}
	default:
		return fmt.Errorf("auth.mode: unsupported mode %q", c.Auth.Mode)
	}
	if c.Auth.AttemptTokenKey != "" && len(c.Auth.AttemptTokenKey) < 32 {
		return fmt.Errorf("auth.attemptTokenKey: must be at least 32 characters")
	}
	if c.Execution.DefaultIsolation != "" && !isolationValid(c.Execution.DefaultIsolation) {
		return fmt.Errorf("execution.defaultIsolation: unknown tier")
	}
	if c.Execution.MaxIsolation != "" && !isolationValid(c.Execution.MaxIsolation) {
		return fmt.Errorf("execution.maxIsolation: unknown tier")
	}
	for _, tier := range c.Execution.SupportedIsolationTiers {
		if !isolationValid(tier) {
			return fmt.Errorf("execution.supportedIsolationTiers: unknown tier %q", tier)
		}
		if tier == "untrusted-gvisor" || tier == "untrusted-vm" {
			if c.Execution.RuntimeClass == "" {
				return fmt.Errorf("execution.supportedIsolationTiers: %s requires execution.runtimeClass", tier)
			}
		}
	}
	for _, toleration := range c.Execution.Tolerations {
		switch toleration.Effect {
		case "", "NoSchedule", "PreferNoSchedule", "NoExecute":
		default:
			return fmt.Errorf("execution.tolerations: unknown effect %q", toleration.Effect)
		}
		switch toleration.Operator {
		case "", "Equal", "Exists":
		default:
			return fmt.Errorf("execution.tolerations: unknown operator %q", toleration.Operator)
		}
	}
	if c.Scheduler.ShardCount <= 0 || c.Scheduler.ShardCount > 1024 {
		return fmt.Errorf("scheduler.shardCount: must be between 1 and 1024")
	}
	if c.Scheduler.ActionCacheTTL < 0 {
		return fmt.Errorf("scheduler.actionCacheTtl: must not be negative")
	}
	pluginNames := map[string]struct{}{}
	for _, plugin := range c.Execution.Plugins {
		if plugin.Name == "" || len(plugin.Command) == 0 {
			return fmt.Errorf("execution.plugins: name and command are required")
		}
		if _, duplicate := pluginNames[plugin.Name]; duplicate {
			return fmt.Errorf("execution.plugins: duplicate plugin %q", plugin.Name)
		}
		pluginNames[plugin.Name] = struct{}{}
		if len(plugin.Languages) == 0 || len(plugin.StepKinds) == 0 {
			return fmt.Errorf("execution.plugins[%s]: languages and stepKinds are required", plugin.Name)
		}
		for _, kind := range plugin.StepKinds {
			if !domain.StepKind(kind).Valid() {
				return fmt.Errorf("execution.plugins[%s]: unknown step kind %q", plugin.Name, kind)
			}
		}
		if plugin.MaxFrameBytes < 0 {
			return fmt.Errorf("execution.plugins[%s]: maxFrameBytes must not be negative", plugin.Name)
		}
	}
	if c.Execution.WorkspaceSizeGiB < 0 {
		return fmt.Errorf("execution.workspaceSizeGiB: must not be negative")
	}
	if c.Scheduler.LeaseTTL < 15*time.Second {
		return fmt.Errorf("scheduler.leaseTtl: must be at least 15s")
	}
	if c.Limits.MaxStepCount <= 0 || c.Limits.MaxParallelSteps <= 0 {
		return fmt.Errorf("limits: step counts must be positive")
	}
	if c.Telemetry.LogLevel == "" {
		return fmt.Errorf("telemetry.logLevel: must not be empty")
	}
	if c.Signing.PrivateKeyFile != "" && strings.ContainsRune(c.Signing.PrivateKeyFile, 0) {
		return fmt.Errorf("signing.privateKeyFile: must not contain NUL")
	}
	return nil
}

func isolationValid(tier string) bool {
	switch tier {
	case "trusted-local", "isolated-container", "untrusted-gvisor", "untrusted-vm":
		return true
	}
	return false
}
