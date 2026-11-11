package e2e

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/actioncache"
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

const testPolicy = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: e2e-local
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
    workerImage: patchproof-worker:test
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

func TestLocalVerificationProducesVerifiableReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping toolchain-backed end-to-end test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain is not available")
	}
	repository := newFixtureRepository(t)
	cfg := localConfig(t, repository)
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
	runCtx, stopRun := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- app.Run(runCtx) }()
	defer func() {
		stopRun()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("application did not stop promptly")
		}
	}()

	tenant, err := app.Admin.GetTenant(ctx, mustTenantID(t, cfg.Auth.DevTenantID))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := app.Admin.PutPolicy(ctx, tenant.ID, "", "e2e", []byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	project, err := app.Admin.CreateProject(ctx, tenant.ID, "e2e", admin.ProjectInput{
		Provider: "git", RepositoryURI: (&url.URL{Scheme: "file", Path: repository}).String(),
		DefaultBranch: "main", DefaultPolicyDigest: policy.Digest, AllowedRegions: []string{"local"},
		Classification: "trusted-local",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := gitOutput(t, repository, "rev-parse", "HEAD~1")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	run, err := app.Runs.Submit(ctx, runservice.SubmitInput{
		TenantID: tenant.ID, ProjectID: project.ID, RequestKey: "e2e-run",
		BaseCommit: base, HeadCommit: head, PolicyDigest: policy.Digest, CreatedBy: "e2e",
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitForTerminal(t, ctx, app, tenant.ID, run.ID)
	if final.Status != domain.RunCompleted {
		t.Fatalf("expected COMPLETED, got %s", final.Status)
	}
	report, err := app.Runs.Report(ctx, tenant.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != domain.VerdictVerified {
		t.Fatalf("expected VERIFIED_WITHIN_SCOPE, got %s", report.Verdict)
	}
	document, err := domain.ParsePolicyDocument([]byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := signing.LoadPublicKey(strings.TrimSuffix(cfg.Signing.PrivateKeyFile, filepath.Ext(cfg.Signing.PrivateKeyFile)) + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	trust := evidence.NewTrustStore()
	if err := trust.Add(evidence.TrustedKey{KeyID: cfg.Signing.KeyID, Identity: "e2e", PublicKey: publicKey}); err != nil {
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
	dag, err := app.Runs.DAG(ctx, tenant.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cached := 0
	for _, step := range dag.Steps {
		if !actioncache.Cacheable(step) {
			continue
		}
		key, err := actioncache.KeyForStep(step, final, cfg.Service.Version, cfg.Execution.WorkerImage)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.Store.GetActionCache(ctx, tenant.ID, key, time.Now().UTC()); err != nil {
			t.Fatalf("cacheable step %s did not produce an action cache entry: %v", step.Key, err)
		}
		cached++
	}
	if cached == 0 {
		t.Fatal("expected at least one cacheable step")
	}
}

func waitForTerminal(t *testing.T, ctx context.Context, app *bootstrap.App, tenantID domain.TenantID, runID domain.RunID) domain.Run {
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
			t.Fatalf("context cancelled while waiting for run: %v", ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Fatal("run did not reach a terminal state")
	return domain.Run{}
}

func localConfig(t *testing.T, repository string) config.Config {
	t.Helper()
	cfg := config.Default()
	stateDir := filepath.Join(t.TempDir(), "state")
	cfg.Service = config.ServiceConfig{Role: config.RoleAllInOne, Name: "patchproof-e2e", Region: "local", Version: "test"}
	cfg.Database.Driver = "memory"
	cfg.Events.Driver = "memory"
	cfg.Events.URLs = nil
	cfg.Artifacts.Driver = "fs"
	cfg.Artifacts.Root = filepath.Join(stateDir, "cas")
	cfg.Execution.DefaultIsolation = string(domain.IsolationTrustedLocal)
	cfg.Execution.MaxIsolation = string(domain.IsolationTrustedLocal)
	cfg.Execution.LocalWorkDir = filepath.Join(stateDir, "work")
	cfg.Worker.WorkDir = filepath.Join(stateDir, "work")
	cfg.Execution.MaxConcurrent = 4
	cfg.Execution.AllowedLanguages = []string{"go"}
	cfg.Execution.EnforceTenantFloor = false
	cfg.Scheduler.ShardCount = 2
	cfg.Scheduler.BatchSize = 4
	cfg.Scheduler.PollInterval = 100 * time.Millisecond
	cfg.Scheduler.LeaseTTL = 30 * time.Second
	cfg.Scheduler.MaxStepsPerTenant = 16
	cfg.Scheduler.MaxStepsPerCluster = 4
	cfg.Telemetry.MetricsAddr = ""
	cfg.Telemetry.LogLevel = "error"
	cfg.Telemetry.LogFormat = "text"
	cfg.Signing.KeyID = "e2e"
	cfg.Signing.PrivateKeyFile = filepath.Join(stateDir, "keys", "signing.pem")
	cfg.Signing.Require = false
	cfg.Auth.Mode = "dev"
	cfg.Auth.DevTenantID = "11111111-1111-4111-8111-111111111111"
	cfg.Auth.DevSubject = "e2e"
	cfg.Auth.DevToken = strings.Repeat("t", 40)
	cfg.Auth.AttemptTokenKey = strings.Repeat("k", 40)
	cfg.Auth.DevRoles = []string{"admin", "operator", "viewer", "platform-admin"}
	for _, directory := range []string{stateDir, cfg.Execution.LocalWorkDir} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	_ = repository
	return cfg
}

func newFixtureRepository(t *testing.T) string {
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
	write("go.mod", "module example.com/e2e\n\ngo 1.23\n")
	write("calc.go", "package e2e\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	write("calc_test.go", "package e2e\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"unexpected sum\")\n\t}\n}\n")
	gitRun(t, repository, "init", "-q")
	gitRun(t, repository, "config", "user.email", "e2e@example.com")
	gitRun(t, repository, "config", "user.name", "e2e")
	gitRun(t, repository, "add", "-A")
	gitRun(t, repository, "commit", "-qm", "base")
	write("calc.go", "package e2e\n\nfunc Add(a, b int) int {\n\treturn b + a\n}\n\nfunc Multiply(a, b int) int {\n\treturn a * b\n}\n")
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

func mustTenantID(t *testing.T, raw string) domain.TenantID {
	t.Helper()
	id, err := domain.ParseTenantID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
