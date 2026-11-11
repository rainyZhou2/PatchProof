package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/cas/localfs"
	"github.com/patchproof/patchproof/internal/bootstrap"
	"github.com/patchproof/patchproof/internal/broker"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/platform/logging"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/internal/worker"
)

const offlineArtifactRootEnv = "PATCHPROOF_OFFLINE_ARTIFACT_ROOT"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "patchproof-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv("PATCHPROOF_CONFIG"))
	if err != nil {
		return err
	}
	logger, err := logging.New(cfg.Telemetry, cfg.Service)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	specPath := firstNonEmpty(os.Getenv("PATCHPROOF_SPEC_FILE"), cfg.Worker.StepSpecFile, "/run/spec/step.json")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("read step spec: %w", err)
	}
	var spec domain.StepSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("decode step spec: %w", err)
	}
	spec.AttemptToken = os.Getenv("PATCHPROOF_ATTEMPT_TOKEN")
	spec.CallbackURL = firstNonEmpty(os.Getenv("PATCHPROOF_CALLBACK_URL"), cfg.Worker.ControllerURL)
	if spec.Deadline.IsZero() {
		spec.Deadline = time.Now().Add(cfg.Execution.ActiveDeadline)
	}

	offlineRoot := strings.TrimSpace(os.Getenv(offlineArtifactRootEnv))
	artifacts, err := openArtifactStore(ctx, cfg, offlineRoot)
	if err != nil {
		return err
	}
	engines, err := bootstrap.NewEngines(cfg)
	if err != nil {
		return err
	}
	runner, err := bootstrap.NewWorker(cfg, artifacts, engines, logger)
	if err != nil {
		return err
	}
	ref := domain.AttemptRef{
		TenantID: spec.TenantID, RunID: spec.RunID, StepID: spec.StepID,
		AttemptID: spec.AttemptID, Generation: spec.Generation,
	}
	// Offline brokered mode: no network, no attempt token, no callback.
	if offlineRoot != "" {
		return runOfflineBrokered(ctx, runner, spec, ref, offlineRoot)
	}
	if spec.CallbackURL == "" {
		return runOffline(ctx, cfg, runner, spec, ref)
	}
	client, err := worker.NewClient(worker.ClientConfig{
		BaseURL: spec.CallbackURL, Timeout: 30 * time.Second, MaxRetries: cfg.Worker.UploadRetries,
	})
	if err != nil {
		return err
	}
	if err := client.Start(ctx, ref, spec.AttemptToken); err != nil {
		return fmt.Errorf("report start: %w", err)
	}
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go func() {
		ticker := time.NewTicker(cfg.Worker.Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if err := client.Heartbeat(heartbeatCtx, ref, spec.AttemptToken); err != nil {
					logger.Warn("attempt heartbeat failed", "error", err.Error())
				}
			}
		}
	}()
	result, runErr := runner.RunStep(ctx, spec)
	cancelHeartbeat()
	if runErr != nil {
		if failErr := client.Fail(ctx, ref, spec.AttemptToken, domain.ErrEngineCrash, runErr.Error(), true); failErr != nil {
			return fmt.Errorf("execution failed (%v) and failure report failed: %w", runErr, failErr)
		}
		return fmt.Errorf("execution failed: %w", runErr)
	}
	if err := client.Complete(ctx, ref, spec.AttemptToken, result); err != nil {
		return fmt.Errorf("report result: %w", err)
	}
	return nil
}

// openArtifactStore selects the attempt-local CAS when brokered offline mode is
// active and otherwise preserves the configured remote store.
func openArtifactStore(ctx context.Context, cfg config.Config, offlineRoot string) (ports.ArtifactStore, error) {
	if strings.TrimSpace(offlineRoot) != "" {
		return localfs.New(offlineRoot)
	}
	return bootstrap.OpenArtifacts(ctx, cfg)
}

func runOfflineBrokered(ctx context.Context, runner *worker.Runner, spec domain.StepSpec, ref domain.AttemptRef, localRoot string) error {
	result, runErr := runner.RunStep(ctx, spec)
	if err := broker.WriteResult(localRoot, ref, result, runErr); err != nil {
		if runErr != nil {
			return fmt.Errorf("execution failed (%v) and result write failed: %w", runErr, err)
		}
		return fmt.Errorf("write offline result: %w", err)
	}
	// A runner error is recorded in the result document and reported by the
	// trusted stage-out phase, which holds the attempt token. Exiting non-zero
	// here would mark the execute Job failed and prevent stage-out from running.
	return nil
}

func runOffline(ctx context.Context, cfg config.Config, runner *worker.Runner, spec domain.StepSpec, ref domain.AttemptRef) error {
	outputDir := cfg.Worker.OfflineOutputDir
	if outputDir == "" {
		return fmt.Errorf("offline worker requires PATCHPROOF_WORKER_OFFLINE_OUTPUT_DIR or %s", offlineArtifactRootEnv)
	}
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return err
	}
	result, runErr := runner.RunStep(ctx, spec)
	document := map[string]any{"attempt_ref": ref, "result": result}
	if runErr != nil {
		document["error"] = runErr.Error()
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, "result.json"), encoded, 0o600); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
