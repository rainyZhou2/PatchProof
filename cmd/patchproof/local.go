package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

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

type localVerifyOptions struct {
	Repository string
	Base       string
	Head       string
	PolicyPath string
	Format     string
	Timeout    time.Duration
	Verbose    bool
}

func runLocalVerify(ctx context.Context, options localVerifyOptions) error {
	repository, err := filepath.Abs(options.Repository)
	if err != nil {
		return err
	}
	baseCommit, err := resolveCommit(ctx, repository, options.Base)
	if err != nil {
		return err
	}
	headCommit, err := resolveCommit(ctx, repository, options.Head)
	if err != nil {
		return err
	}
	policyRaw, err := os.ReadFile(options.PolicyPath)
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	document, err := domain.ParsePolicyDocument(policyRaw)
	if err != nil {
		return fmt.Errorf("parse policy: %w", err)
	}
	if document.Spec.Execution.IsolationTier.Rank() > domain.IsolationTrustedLocal.Rank() {
		return fmt.Errorf("local mode executes trusted code only: policy isolationTier must be %q", domain.IsolationTrustedLocal)
	}
	if document.Spec.Execution.ArtifactTransport == domain.TransportBrokeredOffline {
		return fmt.Errorf("local mode does not provide brokered offline IO")
	}
	cfg, err := localConfig(repository, options.Verbose)
	if err != nil {
		return err
	}
	logger, err := logging.New(cfg.Telemetry, cfg.Service)
	if err != nil {
		return err
	}
	metrics := telemetry.NewMetrics()
	app, err := bootstrap.NewApp(ctx, cfg, logger, metrics, nil, bootstrap.AppOptions{
		Role: config.RoleAllInOne, EnableLocalCluster: true, DisableHTTP: true,
	})
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(runCtx) }()
	defer func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
		}
	}()

	tenant, err := app.Admin.CreateTenant(ctx, "cli", admin.TenantInput{
		HomeRegion: cfg.Service.Region, SecurityTier: domain.SecurityTierStandard, DisplayName: "local",
		Quota: domain.TenantQuota{
			MaxConcurrentRuns: 32, MaxConcurrentSteps: 64, CPUCoreSecondsDay: 200000,
			FuzzSecondsDay: 100000, StorageBytes: 10 << 30, APIRPS: 1000,
		},
	})
	if err != nil {
		return err
	}
	policy, err := app.Admin.PutPolicy(ctx, tenant.ID, "", "cli", policyRaw)
	if err != nil {
		return err
	}
	repositoryURL := (&url.URL{Scheme: "file", Path: repository}).String()
	project, err := app.Admin.CreateProject(ctx, tenant.ID, "cli", admin.ProjectInput{
		Provider: "git", RepositoryURI: repositoryURL, DefaultBranch: "main",
		DefaultPolicyDigest: policy.Digest, AllowedRegions: []string{cfg.Service.Region},
		Classification: "trusted-local",
	})
	if err != nil {
		return err
	}
	run, err := app.Runs.Submit(ctx, runservice.SubmitInput{
		TenantID: tenant.ID, ProjectID: project.ID, RequestKey: "local-" + uuid.NewString(),
		BaseCommit: baseCommit, HeadCommit: headCommit, PolicyDigest: policy.Digest,
		PolicyRef: "trusted://project/default@" + policy.Digest, CreatedBy: "cli",
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "run %s submitted (%s..%s)\n", run.ID, short(baseCommit), short(headCommit))
	final, err := waitForRun(ctx, app, tenant.ID, run.ID, options.Timeout, options.Verbose)
	if err != nil {
		return err
	}
	report, err := app.Runs.Report(ctx, tenant.ID, final.ID)
	if err != nil {
		return fmt.Errorf("fetch report: %w", err)
	}
	reporter, ok := app.Reporters[options.Format]
	if !ok {
		return fmt.Errorf("unknown report format %q", options.Format)
	}
	rendered, err := reporter.Render(ctx, report, nil)
	if err != nil {
		return err
	}
	fmt.Println(string(rendered))
	fmt.Fprintf(os.Stderr, "\nverdict: %s (%s)\n", report.Verdict, final.Status)

	verifyResult, err := verifyLocalReport(ctx, app, report, document, cfg)
	if err != nil {
		return fmt.Errorf("offline verification failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "integrity: %s\n", verifyResult.Status)
	for _, issue := range verifyResult.Issues {
		fmt.Fprintf(os.Stderr, "  issue: %s\n", issue)
	}
	if verifyResult.Status != evidence.StatusValidIntegrity {
		return fmt.Errorf("report integrity check returned %s", verifyResult.Status)
	}
	switch report.Verdict {
	case domain.VerdictCounterexample, domain.VerdictPolicyBlocked, domain.VerdictInfraFailed:
		return fmt.Errorf("verification concluded %s", report.Verdict)
	}
	return nil
}

func verifyLocalReport(ctx context.Context, app *bootstrap.App, report domain.Report, document domain.PolicyDocument, cfg config.Config) (evidence.VerificationResult, error) {
	publicPath := strings.TrimSuffix(cfg.Signing.PrivateKeyFile, filepath.Ext(cfg.Signing.PrivateKeyFile)) + ".pub"
	publicKey, err := signing.LoadPublicKey(publicPath)
	if err != nil {
		return evidence.VerificationResult{}, err
	}
	keyID := cfg.Signing.KeyID
	if keyID == "" {
		keyID = "patchproof-default"
	}
	trust := evidence.NewTrustStore()
	if err := trust.Add(evidence.TrustedKey{KeyID: keyID, Identity: keyID, PublicKey: publicKey}); err != nil {
		return evidence.VerificationResult{}, err
	}
	source := &proofverify.MultiScopeSource{Store: app.Artifacts, TenantID: report.TenantID, Region: report.Provenance.Region}
	return evidence.VerifyReport(ctx, report, source, evidence.VerifyOptions{
		Trust: trust, Now: time.Now().UTC(), RequireSignature: true, Deep: true, Policy: &document,
	})
}

func waitForRun(ctx context.Context, app *bootstrap.App, tenantID domain.TenantID, runID domain.RunID, timeout time.Duration, verbose bool) (domain.Run, error) {
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	lastStatus := domain.RunStatus("")
	polls := 0
	for {
		polls++
		run, err := app.Runs.Get(waitCtx, tenantID, runID)
		if err != nil {
			return domain.Run{}, err
		}
		if verbose && run.Status != lastStatus {
			fmt.Fprintf(os.Stderr, "status: %s\n", run.Status)
			lastStatus = run.Status
		}
		if verbose && polls%5 == 0 {
			if dag, dagErr := app.Runs.DAG(waitCtx, tenantID, runID); dagErr == nil {
				for _, step := range dag.Steps {
					fmt.Fprintf(os.Stderr, "  step %-20s %-10s gen=%d attempts=%d cluster=%s err=%s\n",
						step.Key, step.Status, step.Generation, step.AttemptCount, step.TargetCluster, step.FailureMessage)
				}
			}
		}
		if run.Status.Terminal() {
			return run, nil
		}
		select {
		case <-waitCtx.Done():
			return domain.Run{}, fmt.Errorf("run did not complete within %s (last status %s)", timeout, run.Status)
		case <-ticker.C:
		}
	}
}

func localConfig(repository string, verbose bool) (config.Config, error) {
	cfg := config.Default()
	cfg.Service = config.ServiceConfig{Role: config.RoleAllInOne, Name: "patchproof-local", Region: "local", Version: version}
	cfg.Database.Driver = "memory"
	cfg.Database.DSN = ""
	cfg.Events.Driver = "memory"
	cfg.Events.URLs = nil
	stateDir := filepath.Join(repository, ".patchproof")
	cfg.Artifacts.Driver = "fs"
	cfg.Artifacts.Root = filepath.Join(stateDir, "cas")
	cfg.Execution.DefaultIsolation = string(domain.IsolationTrustedLocal)
	cfg.Execution.MaxIsolation = string(domain.IsolationTrustedLocal)
	cfg.Execution.LocalWorkDir = filepath.Join(stateDir, "work")
	cfg.Worker.WorkDir = filepath.Join(stateDir, "work")
	cfg.Execution.MaxConcurrent = 8
	cfg.Execution.AllowedLanguages = []string{"go"}
	cfg.Execution.EnforceTenantFloor = false
	cfg.Scheduler.ShardCount = 4
	cfg.Scheduler.BatchSize = 8
	cfg.Scheduler.PollInterval = 200 * time.Millisecond
	cfg.Scheduler.LeaseTTL = 45 * time.Second
	cfg.Scheduler.MaxStepsPerTenant = 32
	cfg.Scheduler.MaxStepsPerCluster = 8
	cfg.Telemetry.MetricsAddr = ""
	cfg.Telemetry.LogLevel = "warn"
	cfg.Telemetry.LogFormat = "text"
	cfg.Signing.Algorithm = "ed25519"
	cfg.Signing.KeyID = "patchproof-local"
	cfg.Signing.PrivateKeyFile = filepath.Join(stateDir, "keys", "signing.pem")
	cfg.Signing.Require = true
	cfg.Auth.Mode = "dev"
	cfg.Auth.DevTenantID = uuid.NewString()
	cfg.Auth.DevSubject = "patchproof-local"
	devToken, err := randomHex(32)
	if err != nil {
		return config.Config{}, err
	}
	attemptKey, err := randomHex(32)
	if err != nil {
		return config.Config{}, err
	}
	cfg.Auth.DevToken = devToken
	cfg.Auth.AttemptTokenKey = attemptKey
	cfg.Auth.AttemptTokenTTL = 30 * time.Minute
	cfg.Auth.DevRoles = []string{"admin", "operator", "viewer", "platform-admin"}
	if verbose {
		cfg.Telemetry.LogLevel = "debug"
	}
	for _, directory := range []string{stateDir, cfg.Execution.LocalWorkDir} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return config.Config{}, err
		}
	}
	return cfg, nil
}

func resolveCommit(ctx context.Context, repository, reference string) (string, error) {
	if reference == "" {
		reference = "HEAD"
	}
	command := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "--verify", reference+"^{commit}")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", reference, err)
	}
	commit := strings.TrimSpace(string(output))
	if len(commit) != 40 && len(commit) != 64 {
		return "", fmt.Errorf("resolve %s: unexpected commit %q", reference, commit)
	}
	return commit, nil
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
