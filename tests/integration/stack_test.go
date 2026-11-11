package integration

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/app/admin"
	"github.com/patchproof/patchproof/internal/app/runservice"
	"github.com/patchproof/patchproof/internal/bootstrap"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/platform/logging"
	"github.com/patchproof/patchproof/internal/platform/signing"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/proofverify"
	"github.com/patchproof/patchproof/pkg/evidence"
)

const integrationPolicy = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: integration
spec:
  trustedSource:
    type: project-admin
    requireSignedRevision: false
  sources:
    maxRepositorySizeGiB: 1
    maxChangedFiles: 100
    maxFileSizeMiB: 8
    disallowSubmodulesUnlessApproved: true
  verification:
    required: [compile, regression-test]
    optional: []
    allowInconclusiveOn: []
    minimumEvidenceLevels: {}
  execution:
    isolationTier: trusted-local
    allowedRegions: [local]
    defaultCpuMilli: 1000
    defaultMemoryMiB: 1024
    defaultDiskGiB: 2
    maxStepWallSeconds: 300
    maxRunWallSeconds: 600
    subjectNetwork: none
    artifactTransport: direct
    workerImage: patchproof-worker:integration
  budgets:
    totalCpuSeconds: 600
    fuzzCpuSeconds: 60
    maxParallelSteps: 4
    maxStepCount: 16
  protectedPaths: []
  report:
    formats: [json, markdown, sarif]
    requireAttestation: true
    includeCounterexamples: true
`

// TestPostgresStack verifies the full all-in-one pipeline against a real
// PostgreSQL instance. It is skipped unless PATCHPROOF_TEST_INTEGRATION=1 and
// PATCHPROOF_TEST_POSTGRES_DSN are set.
func TestPostgresStack(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in short mode")
	}
	if os.Getenv("PATCHPROOF_TEST_INTEGRATION") != "1" {
		t.Skip("set PATCHPROOF_TEST_INTEGRATION=1 to run stack integration tests")
	}
	dsn := os.Getenv("PATCHPROOF_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PATCHPROOF_TEST_POSTGRES_DSN is not set")
	}
	for _, binary := range []string{"git", "go"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is not available", binary)
		}
	}
	repository := fixtureRepository(t)
	root := t.TempDir()
	cfg := config.Default()
	cfg.Service = config.ServiceConfig{Role: config.RoleAllInOne, Name: "patchproof-integration", Region: "local", Version: "test"}
	cfg.Database.Driver = "postgres"
	cfg.Database.DSN = dsn
	cfg.Database.MigrateOnStart = true
	cfg.Events.Driver = "memory"
	cfg.Events.URLs = nil
	cfg.Artifacts.Driver = "fs"
	cfg.Artifacts.Root = filepath.Join(root, "cas")
	cfg.Execution.DefaultIsolation = string(domain.IsolationTrustedLocal)
	cfg.Execution.MaxIsolation = string(domain.IsolationTrustedLocal)
	cfg.Execution.EnforceTenantFloor = false
	cfg.Execution.AllowFileRepositories = true
	cfg.Execution.AllowFullFetch = true
	cfg.Execution.LocalWorkDir = filepath.Join(root, "work")
	cfg.Worker.WorkDir = filepath.Join(root, "work")
	cfg.Execution.MaxConcurrent = 4
	cfg.Execution.AllowedLanguages = []string{"go"}
	cfg.Scheduler.ShardCount = 2
	cfg.Scheduler.BatchSize = 4
	cfg.Scheduler.PollInterval = 100 * time.Millisecond
	cfg.Scheduler.LeaseTTL = 30 * time.Second
	cfg.Scheduler.MaxStepsPerTenant = 16
	cfg.Scheduler.MaxStepsPerCluster = 4
	cfg.Telemetry.MetricsAddr = ""
	cfg.Telemetry.LogLevel = "error"
	cfg.Signing.KeyID = "integration"
	cfg.Signing.PrivateKeyFile = filepath.Join(root, "keys", "signing.pem")
	cfg.Auth.Mode = "dev"
	cfg.Auth.DevTenantID = "33333333-3333-4333-8333-333333333333"
	cfg.Auth.DevSubject = "integration"
	cfg.Auth.DevToken = strings.Repeat("i", 40)
	cfg.Auth.AttemptTokenKey = strings.Repeat("k", 40)
	for _, directory := range []string{cfg.Execution.LocalWorkDir, filepath.Dir(cfg.Signing.PrivateKeyFile)} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logger, err := logging.New(cfg.Telemetry, cfg.Service)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	app, err := bootstrap.NewApp(ctx, cfg, logger, telemetry.NewMetrics(), nil, bootstrap.AppOptions{
		Role: config.RoleAllInOne, EnableLocalCluster: true, DisableHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- app.Run(runCtx) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()
	tenantID, err := domain.ParseTenantID(cfg.Auth.DevTenantID)
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := app.Admin.GetTenant(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := app.Admin.PutPolicy(ctx, tenant.ID, "", "integration", []byte(integrationPolicy))
	if err != nil {
		t.Fatal(err)
	}
	project, err := app.Admin.CreateProject(ctx, tenant.ID, "integration", admin.ProjectInput{
		Provider: "git", RepositoryURI: (&url.URL{Scheme: "file", Path: repository}).String(),
		DefaultBranch: "main", DefaultPolicyDigest: policy.Digest, AllowedRegions: []string{"local"},
		Classification: "trusted-local",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := app.Runs.Submit(ctx, runservice.SubmitInput{
		TenantID: tenant.ID, ProjectID: project.ID, RequestKey: "integration-" + time.Now().Format(time.RFC3339Nano),
		BaseCommit: gitOutput(t, repository, "rev-parse", "HEAD~1"), HeadCommit: gitOutput(t, repository, "rev-parse", "HEAD"),
		PolicyDigest: policy.Digest, CreatedBy: "integration",
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, ctx, app, tenant.ID, run.ID)
	if final.Verdict != domain.VerdictVerified {
		t.Fatalf("expected VERIFIED_WITHIN_SCOPE, got %s (%s)", final.Verdict, final.Status)
	}
	report, err := app.Runs.Report(ctx, tenant.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := signing.LoadPublicKey(strings.TrimSuffix(cfg.Signing.PrivateKeyFile, filepath.Ext(cfg.Signing.PrivateKeyFile)) + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	trust := evidence.NewTrustStore()
	if err := trust.Add(evidence.TrustedKey{KeyID: cfg.Signing.KeyID, Identity: "integration", PublicKey: publicKey}); err != nil {
		t.Fatal(err)
	}
	document, err := domain.ParsePolicyDocument([]byte(integrationPolicy))
	if err != nil {
		t.Fatal(err)
	}
	source := &proofverify.MultiScopeSource{Store: app.Artifacts, TenantID: report.TenantID, Region: report.Provenance.Region}
	result, err := evidence.VerifyReport(ctx, report, source, evidence.VerifyOptions{
		Trust: trust, RequireSignature: true, Deep: true, Policy: &document, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != evidence.StatusValidIntegrity {
		t.Fatalf("expected VALID_INTEGRITY, got %s: %v", result.Status, result.Issues)
	}
}

func waitTerminal(t *testing.T, ctx context.Context, app *bootstrap.App, tenantID domain.TenantID, runID domain.RunID) domain.Run {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		run, err := app.Runs.Get(ctx, tenantID, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status.Terminal() {
			return run
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatal("run did not complete")
	return domain.Run{}
}

func fixtureRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	write := func(name, content string) {
		path := filepath.Join(repository, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/integration\n\ngo 1.23\n")
	write("calc.go", "package integration\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	write("calc_test.go", "package integration\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"unexpected sum\")\n\t}\n}\n")
	gitRun(t, repository, "init", "-q")
	gitRun(t, repository, "config", "user.email", "integration@example.com")
	gitRun(t, repository, "config", "user.name", "integration")
	gitRun(t, repository, "add", "-A")
	gitRun(t, repository, "commit", "-qm", "base")
	write("calc.go", "package integration\n\nfunc Add(a, b int) int {\n\treturn b + a\n}\n\nfunc Multiply(a, b int) int {\n\treturn a * b\n}\n")
	gitRun(t, repository, "add", "-A")
	gitRun(t, repository, "commit", "-qm", "head")
	return repository
}

func gitRun(t *testing.T, repository string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func gitOutput(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}
