package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// isolatePatchProofEnv removes every PATCHPROOF_* variable for the duration of
// a test so that Load is not affected by the developer's shell.
func isolatePatchProofEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "PATCHPROOF_") {
			continue
		}
		old, had := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, old)
				return
			}
			_ = os.Unsetenv(key)
		})
	}
}

func validConfig() Config {
	cfg := Default()
	cfg.Service.Role = RoleController
	cfg.Database.DSN = "postgres://user:pass@localhost:5432/patchproof"
	cfg.Events.URLs = []string{"nats://127.0.0.1:4222"}
	return cfg
}

func TestDefaultValues(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Service.Name != "patchproof" || cfg.Service.Region != "region-a" {
		t.Fatalf("unexpected service defaults: %+v", cfg.Service)
	}
	if cfg.HTTP.ListenAddr != ":8080" || cfg.HTTP.MaxBodyBytes != 2<<20 {
		t.Fatalf("unexpected http defaults: %+v", cfg.HTTP)
	}
	if cfg.Database.Driver != "postgres" || cfg.Database.MaxConns != 20 || cfg.Database.MinConns != 2 {
		t.Fatalf("unexpected database defaults: %+v", cfg.Database)
	}
	if cfg.Events.Driver != "nats" || cfg.Events.Stream != "PATCHPROOF" {
		t.Fatalf("unexpected events defaults: %+v", cfg.Events)
	}
	if cfg.Artifacts.Driver != "fs" || cfg.Artifacts.Root == "" {
		t.Fatalf("unexpected artifacts defaults: %+v", cfg.Artifacts)
	}
	if cfg.Auth.Mode != "dev" || cfg.Auth.TenantClaim != "tenant_id" || cfg.Auth.RolesClaim != "roles" {
		t.Fatalf("unexpected auth defaults: %+v", cfg.Auth)
	}
	if cfg.Execution.DefaultIsolation != "trusted-local" || !cfg.Execution.EnforceTenantFloor {
		t.Fatalf("unexpected execution defaults: %+v", cfg.Execution)
	}
	if cfg.Scheduler.ShardCount != 16 || cfg.Scheduler.LeaseTTL == 0 {
		t.Fatalf("unexpected scheduler defaults: %+v", cfg.Scheduler)
	}
	if cfg.Telemetry.LogLevel != "info" || cfg.Telemetry.LogFormat != "json" {
		t.Fatalf("unexpected telemetry defaults: %+v", cfg.Telemetry)
	}
	if cfg.Limits.MaxParallelSteps <= 0 || cfg.Limits.APIRPSPerTenant <= 0 {
		t.Fatalf("unexpected limits defaults: %+v", cfg.Limits)
	}
	if cfg.State.Driver != "postgres" {
		t.Fatalf("unexpected state defaults: %+v", cfg.State)
	}
}

func TestDefaultConfigRequiresDeploymentEndpoints(t *testing.T) {
	cfg := Default()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("bare defaults validated without a database DSN and NATS URL")
	}
	if !strings.Contains(err.Error(), "database.dsn") {
		t.Fatalf("expected a database.dsn error, got %v", err)
	}
}

func TestDefaultValuesValidateWithRequiredEndpoints(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Database.DSN = "postgres://user:pass@localhost:5432/patchproof"
	cfg.Events.URLs = []string{"nats://127.0.0.1:4222"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults with required endpoints should validate: %v", err)
	}
}

func TestWorkerRoleSkipsDatabaseAndEventRequirements(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Service.Role = RoleWorker
	cfg.Database.Driver = "postgres"
	cfg.Database.DSN = ""
	cfg.Events.Driver = "nats"
	cfg.Events.URLs = nil
	cfg.Auth.Mode = "dev"
	cfg.Auth.DevToken = ""
	cfg.Auth.DevTenantID = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("worker role should not require database/event endpoints: %v", err)
	}
}

func TestLoadYAMLFile(t *testing.T) {
	isolatePatchProofEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := `
service:
  role: controller
  name: yaml-service
  region: yaml-region
database:
  driver: postgres
  dsn: postgres://yaml/db
  maxConns: 10
  minConns: 1
  migrateOnStart: true
events:
  driver: nats
  urls:
    - nats://yaml:4222
artifacts:
  driver: s3
  bucket: yaml-bucket
  region: us-east-1
execution:
  enforceTenantFloor: false
scheduler:
  shardCount: 8
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Service.Role != RoleController || cfg.Service.Name != "yaml-service" || cfg.Service.Region != "yaml-region" {
		t.Fatalf("unexpected service config: %+v", cfg.Service)
	}
	if cfg.Database.DSN != "postgres://yaml/db" || cfg.Database.MaxConns != 10 || !cfg.Database.MigrateOnStart {
		t.Fatalf("unexpected database config: %+v", cfg.Database)
	}
	if len(cfg.Events.URLs) != 1 || cfg.Events.URLs[0] != "nats://yaml:4222" {
		t.Fatalf("unexpected events config: %+v", cfg.Events)
	}
	if cfg.Artifacts.Driver != "s3" || cfg.Artifacts.Bucket != "yaml-bucket" {
		t.Fatalf("unexpected artifacts config: %+v", cfg.Artifacts)
	}
	if cfg.Execution.EnforceTenantFloor {
		t.Fatal("yaml enforceTenantFloor=false was ignored")
	}
	if cfg.Scheduler.ShardCount != 8 {
		t.Fatalf("shard count = %d, want 8", cfg.Scheduler.ShardCount)
	}
	// Values not present in YAML keep their defaults.
	if cfg.HTTP.ListenAddr != ":8080" {
		t.Fatalf("default listen address was not preserved: %q", cfg.HTTP.ListenAddr)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	isolatePatchProofEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := `
service:
  role: controller
  region: yaml-region
database:
  driver: postgres
  dsn: postgres://yaml/db
events:
  driver: nats
  urls: [nats://yaml:4222]
artifacts:
  driver: s3
  bucket: yaml-bucket
execution:
  enforceTenantFloor: false
scheduler:
  shardCount: 8
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("PATCHPROOF_POSTGRES_DSN", "postgres://env/db")
	t.Setenv("PATCHPROOF_S3_BUCKET", "env-bucket")
	t.Setenv("PATCHPROOF_ROLE", "controller")
	t.Setenv("PATCHPROOF_SHARD_COUNT", "32")
	t.Setenv("PATCHPROOF_ENFORCE_TENANT_FLOOR", "true")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.DSN != "postgres://env/db" {
		t.Fatalf("DSN env override not applied: %q", cfg.Database.DSN)
	}
	if cfg.Artifacts.Bucket != "env-bucket" {
		t.Fatalf("bucket env override not applied: %q", cfg.Artifacts.Bucket)
	}
	if cfg.Service.Role != RoleController {
		t.Fatalf("role env override not applied: %q", cfg.Service.Role)
	}
	if cfg.Scheduler.ShardCount != 32 {
		t.Fatalf("shard count env override not applied: %d", cfg.Scheduler.ShardCount)
	}
	if !cfg.Execution.EnforceTenantFloor {
		t.Fatal("boolean env override did not enable enforceTenantFloor")
	}
	if cfg.Service.Region != "yaml-region" {
		t.Fatalf("unrelated value changed: %q", cfg.Service.Region)
	}
}

func TestLoadEnvOverridesRejectMalformedValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{name: "bad boolean", key: "PATCHPROOF_ENFORCE_TENANT_FLOOR", val: "not-a-bool"},
		{name: "bad integer", key: "PATCHPROOF_SHARD_COUNT", val: "many"},
		{name: "bad duration", key: "PATCHPROOF_LEASE_TTL", val: "not-a-duration"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			isolatePatchProofEnv(t)
			t.Setenv(test.key, test.val)
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("Load error = %v, want it to mention %s", err, test.key)
			}
		})
	}
}

func TestNATSURLEnvListing(t *testing.T) {
	isolatePatchProofEnv(t)
	t.Setenv("PATCHPROOF_POSTGRES_DSN", "postgres://env/db")
	t.Setenv("PATCHPROOF_NATS_URL", "nats://one:4222,nats://two:4222")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Role is empty, so the default driver is postgres without a DSN: provide
	// one through the environment for a valid configuration.
	if cfg.Database.Driver != "postgres" {
		t.Fatalf("database driver = %q", cfg.Database.Driver)
	}
	if len(cfg.Events.URLs) != 2 || cfg.Events.URLs[0] != "nats://one:4222" || cfg.Events.URLs[1] != "nats://two:4222" {
		t.Fatalf("NATS URL list = %#v", cfg.Events.URLs)
	}
}

func TestLoadRejectsUnknownYAMLFields(t *testing.T) {
	isolatePatchProofEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := `
service:
  role: controller
  notAField: true
database:
  dsn: postgres://yaml/db
events:
  urls: [nats://yaml:4222]
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown YAML field to be rejected")
	}
}

func TestLoadMissingFile(t *testing.T) {
	isolatePatchProofEnv(t)
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected missing config file error")
	}
}

func TestValidateRejectsInvalidCombinations(t *testing.T) {
	t.Parallel()
	validTenant := domain.NewTenantID().String()
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "postgres without dsn",
			mutate: func(cfg *Config) { cfg.Database.DSN = "" },
			want:   "database.dsn",
		},
		{
			name:   "sqlite without path",
			mutate: func(cfg *Config) { cfg.Database.Driver = "sqlite"; cfg.Database.SQLitePath = "" },
			want:   "database.sqlitePath",
		},
		{
			name:   "unsupported database driver",
			mutate: func(cfg *Config) { cfg.Database.Driver = "mysql" },
			want:   "database.driver",
		},
		{
			name:   "invalid connection pool bounds",
			mutate: func(cfg *Config) { cfg.Database.MaxConns = 1; cfg.Database.MinConns = 2 },
			want:   "connection pool",
		},
		{
			name:   "nats without url",
			mutate: func(cfg *Config) { cfg.Events.URLs = nil },
			want:   "events.urls",
		},
		{
			name:   "nats with empty url",
			mutate: func(cfg *Config) { cfg.Events.URLs = []string{""} },
			want:   "events.urls",
		},
		{
			name:   "unsupported events driver",
			mutate: func(cfg *Config) { cfg.Events.Driver = "kafka" },
			want:   "events.driver",
		},
		{
			name:   "bad shard count zero",
			mutate: func(cfg *Config) { cfg.Scheduler.ShardCount = 0 },
			want:   "scheduler.shardCount",
		},
		{
			name:   "bad shard count too high",
			mutate: func(cfg *Config) { cfg.Scheduler.ShardCount = 1025 },
			want:   "scheduler.shardCount",
		},
		{
			name:   "short lease ttl",
			mutate: func(cfg *Config) { cfg.Scheduler.LeaseTTL = 5 * time.Second },
			want:   "scheduler.leaseTtl",
		},
		{
			name: "dev mode without tenant uuid",
			mutate: func(cfg *Config) {
				cfg.Service.Role = RoleAPI
				cfg.Auth.DevToken = "dev-token"
				cfg.Auth.DevTenantID = "not-a-uuid"
			},
			want: "devTenantId",
		},
		{
			name: "dev mode without dev token",
			mutate: func(cfg *Config) {
				cfg.Service.Role = RoleAPI
				cfg.Auth.DevToken = ""
				cfg.Auth.DevTenantID = validTenant
			},
			want: "devToken",
		},
		{
			name: "oidc without issuer audience and jwks",
			mutate: func(cfg *Config) {
				cfg.Auth.Mode = "oidc"
				cfg.Auth.Issuer = ""
				cfg.Auth.Audience = ""
				cfg.Auth.JWKSURL = ""
			},
			want: "auth:",
		},
		{
			name:   "unsupported auth mode",
			mutate: func(cfg *Config) { cfg.Auth.Mode = "basic" },
			want:   "auth.mode",
		},
		{
			name:   "short attempt token key",
			mutate: func(cfg *Config) { cfg.Auth.AttemptTokenKey = "too-short" },
			want:   "attemptTokenKey",
		},
		{
			name: "untrusted isolation without runtime class",
			mutate: func(cfg *Config) {
				cfg.Execution.RuntimeClass = ""
				cfg.Execution.SupportedIsolationTiers = []string{"untrusted-gvisor"}
			},
			want: "runtimeClass",
		},
		{
			name:   "unknown isolation tier",
			mutate: func(cfg *Config) { cfg.Execution.SupportedIsolationTiers = []string{"magic-sandbox"} },
			want:   "supportedIsolationTiers",
		},
		{
			name:   "unknown toleration effect",
			mutate: func(cfg *Config) { cfg.Execution.Tolerations = []Toleration{{Key: "k", Effect: "Sometimes"}} },
			want:   "tolerations",
		},
		{
			name:   "unsupported artifacts driver",
			mutate: func(cfg *Config) { cfg.Artifacts.Driver = "ftp" },
			want:   "artifacts.driver",
		},
		{
			name:   "fs artifacts without root",
			mutate: func(cfg *Config) { cfg.Artifacts.Root = "" },
			want:   "artifacts.root",
		},
		{
			name:   "s3 artifacts without bucket",
			mutate: func(cfg *Config) { cfg.Artifacts.Driver = "s3"; cfg.Artifacts.Bucket = "" },
			want:   "artifacts.bucket",
		},
		{
			name:   "unknown role",
			mutate: func(cfg *Config) { cfg.Service.Role = Role("hacker") },
			want:   "service.role",
		},
		{
			name:   "empty region",
			mutate: func(cfg *Config) { cfg.Service.Region = "" },
			want:   "service.region",
		},
		{
			name:   "relative external url",
			mutate: func(cfg *Config) { cfg.HTTP.ExternalURL = "/relative" },
			want:   "http.externalUrl",
		},
		{
			name:   "non positive step limits",
			mutate: func(cfg *Config) { cfg.Limits.MaxStepCount = 0 },
			want:   "limits",
		},
		{
			name:   "empty log level",
			mutate: func(cfg *Config) { cfg.Telemetry.LogLevel = "" },
			want:   "telemetry.logLevel",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted invalid config: %+v", cfg)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %q, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestValidateAcceptsRuntimeClassForUntrustedTier(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	cfg.Execution.SupportedIsolationTiers = []string{"untrusted-gvisor", "untrusted-vm"}
	cfg.Execution.RuntimeClass = "gvisor"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateAcceptsMemoryDrivers(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Service.Role = RoleAllInOne
	cfg.Database.Driver = "memory"
	cfg.Database.DSN = ""
	cfg.Events.Driver = "memory"
	cfg.Events.URLs = nil
	cfg.Artifacts.Driver = "memory"
	cfg.Artifacts.Root = ""
	cfg.Auth.DevToken = "dev-token"
	cfg.Auth.DevTenantID = domain.NewTenantID().String()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate memory drivers: %v", err)
	}
}
