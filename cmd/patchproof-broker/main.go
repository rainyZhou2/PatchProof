// Command patchproof-broker performs the trusted stage-in and stage-out halves
// of brokered offline artifact IO. It holds the CAS credentials and the
// attempt token; the executing sandbox holds neither.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/patchproof/patchproof/internal/bootstrap"
	"github.com/patchproof/patchproof/internal/broker"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/platform/logging"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/internal/worker"
)

const (
	phaseStageIn  = "stage-in"
	phaseStageOut = "stage-out"

	callbackTimeout = 30 * time.Second
)

type options struct {
	phase            string
	specFile         string
	localRoot        string
	attemptTokenFile string
	callbackURL      string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "patchproof-broker:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger, err := logging.New(cfg.Telemetry, cfg.Service)
	if err != nil {
		return err
	}
	logger = logger.With(slog.String("component", "patchproof-broker"))
	remote, err := bootstrap.OpenArtifacts(ctx, cfg)
	if err != nil {
		return err
	}
	return execute(ctx, opts, remote, logger)
}

// loadConfig loads configuration from the environment (and PATCHPROOF_CONFIG
// when set). The broker is a trusted worker-role component, so a missing role
// defaults to "worker" to avoid requiring unrelated database/event settings.
func loadConfig() (config.Config, error) {
	if strings.TrimSpace(os.Getenv("PATCHPROOF_ROLE")) == "" {
		if err := os.Setenv("PATCHPROOF_ROLE", string(config.RoleWorker)); err != nil {
			return config.Config{}, fmt.Errorf("set broker role: %w", err)
		}
	}
	cfg, err := config.Load(os.Getenv("PATCHPROOF_CONFIG"))
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

func parseOptions(args []string) (options, error) {
	var opts options
	flags := flag.NewFlagSet("patchproof-broker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.phase, "phase", "", "brokered IO phase: stage-in or stage-out")
	flags.StringVar(&opts.specFile, "spec-file", "", "path to the step spec JSON (defaults to PATCHPROOF_SPEC_FILE)")
	flags.StringVar(&opts.localRoot, "local-root", "", "attempt-scoped local artifact root (defaults to PATCHPROOF_LOCAL_ROOT)")
	flags.StringVar(&opts.attemptTokenFile, "attempt-token-file", "", "file containing the attempt token (stage-out; falls back to PATCHPROOF_ATTEMPT_TOKEN)")
	flags.StringVar(&opts.callbackURL, "callback-url", "", "controller callback base URL (stage-out; falls back to PATCHPROOF_CALLBACK_URL)")
	if err := flags.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}
	if flags.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if opts.specFile == "" {
		opts.specFile = strings.TrimSpace(os.Getenv("PATCHPROOF_SPEC_FILE"))
	}
	if opts.localRoot == "" {
		opts.localRoot = strings.TrimSpace(os.Getenv("PATCHPROOF_LOCAL_ROOT"))
	}
	if opts.callbackURL == "" {
		opts.callbackURL = strings.TrimSpace(os.Getenv("PATCHPROOF_CALLBACK_URL"))
	}
	if err := opts.validate(); err != nil {
		return options{}, err
	}
	return opts, nil
}

func (o options) validate() error {
	switch o.phase {
	case phaseStageIn, phaseStageOut:
	default:
		return fmt.Errorf("--phase must be %q or %q, got %q", phaseStageIn, phaseStageOut, o.phase)
	}
	if strings.TrimSpace(o.specFile) == "" {
		return errors.New("--spec-file is required (or set PATCHPROOF_SPEC_FILE)")
	}
	if strings.TrimSpace(o.localRoot) == "" {
		return errors.New("--local-root is required (or set PATCHPROOF_LOCAL_ROOT)")
	}
	if o.phase == phaseStageOut && strings.TrimSpace(o.callbackURL) == "" {
		return errors.New("--callback-url is required for stage-out (or set PATCHPROOF_CALLBACK_URL)")
	}
	return nil
}

func execute(ctx context.Context, opts options, remote ports.ArtifactStore, logger *slog.Logger) error {
	spec, err := readSpec(opts.specFile)
	if err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	switch opts.phase {
	case phaseStageIn:
		return runStageIn(ctx, opts, spec, remote, logger)
	case phaseStageOut:
		return runStageOut(ctx, opts, spec, remote, logger)
	default:
		return fmt.Errorf("unsupported phase %q", opts.phase)
	}
}

func runStageIn(ctx context.Context, opts options, spec domain.StepSpec, remote ports.ArtifactStore, logger *slog.Logger) error {
	started := time.Now()
	manifest, err := broker.StageIn(ctx, spec, remote, opts.localRoot, worker.ScopeFor(spec))
	if err != nil {
		logger.Error("stage-in failed",
			slog.String("phase", phaseStageIn),
			slog.String("attempt_id", spec.AttemptID.String()),
			slog.String("local_root", opts.localRoot),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("stage-in: %w", err)
	}
	logger.Info("stage-in complete",
		slog.String("phase", phaseStageIn),
		slog.String("attempt_id", spec.AttemptID.String()),
		slog.String("local_root", opts.localRoot),
		slog.Int("inputs", len(manifest.Inputs)),
		slog.Duration("elapsed", time.Since(started)),
	)
	return nil
}

func runStageOut(ctx context.Context, opts options, spec domain.StepSpec, remote ports.ArtifactStore, logger *slog.Logger) error {
	token, err := resolveAttemptToken(opts.attemptTokenFile)
	if err != nil {
		return err
	}
	callbackURL := strings.TrimSpace(opts.callbackURL)
	if callbackURL == "" {
		callbackURL = strings.TrimSpace(os.Getenv("PATCHPROOF_CALLBACK_URL"))
	}
	if callbackURL == "" {
		return errors.New("stage-out requires a callback URL")
	}
	spec.AttemptToken = token
	spec.CallbackURL = callbackURL
	ref := broker.AttemptRefOf(spec)
	client, err := worker.NewClient(worker.ClientConfig{
		BaseURL: callbackURL, Timeout: callbackTimeout, MaxRetries: 4,
	})
	if err != nil {
		return fmt.Errorf("stage-out: %w", err)
	}
	started := time.Now()
	if err := client.Start(ctx, ref, token); err != nil {
		return fmt.Errorf("stage-out: report start: %w", err)
	}
	result, stageErr := broker.StageOut(ctx, spec, opts.localRoot, remote, worker.ScopeFor(spec))
	if stageErr != nil {
		class, message, retryable := broker.Failure(stageErr)
		logger.Error("stage-out failed",
			slog.String("phase", phaseStageOut),
			slog.String("attempt_id", spec.AttemptID.String()),
			slog.String("error_class", string(class)),
			slog.String("error", stageErr.Error()),
		)
		if failErr := client.Fail(ctx, ref, token, class, message, retryable); failErr != nil {
			return fmt.Errorf("stage-out failed (%v) and failure report failed: %w", stageErr, failErr)
		}
		return fmt.Errorf("stage-out: %w", stageErr)
	}
	if err := client.Complete(ctx, ref, token, result); err != nil {
		return fmt.Errorf("stage-out: report result: %w", err)
	}
	logger.Info("stage-out complete",
		slog.String("phase", phaseStageOut),
		slog.String("attempt_id", spec.AttemptID.String()),
		slog.String("outcome", string(result.Outcome)),
		slog.String("local_root", opts.localRoot),
		slog.Duration("elapsed", time.Since(started)),
	)
	return nil
}

func resolveAttemptToken(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read attempt token: %w", err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", errors.New("attempt token file is empty")
		}
		return token, nil
	}
	token := strings.TrimSpace(os.Getenv("PATCHPROOF_ATTEMPT_TOKEN"))
	if token == "" {
		return "", errors.New("stage-out requires --attempt-token-file or PATCHPROOF_ATTEMPT_TOKEN")
	}
	return token, nil
}

func readSpec(path string) (domain.StepSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.StepSpec{}, fmt.Errorf("read step spec: %w", err)
	}
	var spec domain.StepSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return domain.StepSpec{}, fmt.Errorf("decode step spec: %w", err)
	}
	return spec, nil
}
