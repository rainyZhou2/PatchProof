package worker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

type Config struct {
	WorkDir         string
	MaxExtractBytes int64
	MaxFiles        int
	MaxArtifacts    int
	UploadRetries   int
	KeepWorkspace   bool
	Clock           ports.Clock
	Logger          *slog.Logger
}

type Deps struct {
	Engines *engine.Registry
	Store   ports.ArtifactStore
}

type Runner struct {
	config Config
	deps   Deps
}

func New(config Config, deps Deps) (*Runner, error) {
	if deps.Engines == nil {
		return nil, fmt.Errorf("worker: engine registry is required")
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("worker: artifact store is required")
	}
	if config.WorkDir == "" {
		config.WorkDir = os.TempDir()
	}
	if config.MaxExtractBytes <= 0 {
		config.MaxExtractBytes = 8 << 30
	}
	if config.MaxFiles <= 0 {
		config.MaxFiles = 200000
	}
	if config.MaxArtifacts <= 0 {
		config.MaxArtifacts = 256
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Runner{config: config, deps: deps}, nil
}

func (r *Runner) RunStep(ctx context.Context, spec domain.StepSpec) (result domain.StepResult, err error) {
	started := r.config.Clock.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			r.config.Logger.Error("worker panic", slog.String("step_id", spec.StepID.String()), slog.Any("panic", recovered))
			result = domain.StepResult{
				Outcome:    domain.OutcomeFailed,
				ErrorClass: domain.ErrEngineCrash,
				Message:    fmt.Sprintf("engine panicked: %v", recovered),
				Retryable:  false,
				FinishedAt: r.config.Clock.Now(),
			}
			err = nil
		}
	}()
	if err := validateSpec(spec); err != nil {
		return domain.StepResult{
			Outcome:    domain.OutcomeFailed,
			ErrorClass: domain.ErrValidationFailed,
			Message:    err.Error(),
			Retryable:  false,
			FinishedAt: r.config.Clock.Now(),
		}, nil
	}
	workspace, cleanup, err := r.prepareWorkspace(spec)
	if err != nil {
		return domain.StepResult{}, err
	}
	if !r.config.KeepWorkspace {
		defer cleanup()
	}

	scope := ScopeFor(spec)
	staged, err := r.stageInputs(ctx, spec, workspace, scope)
	if err != nil {
		return failedResult(domain.ErrCASUnavailable, "stage inputs: "+err.Error(), true, r.config.Clock.Now()), nil
	}

	selected, ok := r.deps.Engines.ForEngine(spec.Kind, stepopts.Get(spec.Options, stepopts.Language), stepopts.Get(spec.Options, stepopts.Engine))
	if !ok {
		return failedResult(domain.ErrEngineUnsupported, "no engine satisfies kind "+string(spec.Kind)+" and engine "+stepopts.Get(spec.Options, stepopts.Engine), false, r.config.Clock.Now()), nil
	}

	deadline := effectiveDeadline(spec, r.config.Clock.Now())
	execCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	request := engine.Request{
		Spec:      spec,
		TenantID:  spec.TenantID,
		Workspace: workspace,
		Staged:    staged,
		Store:     r.deps.Store,
		Scope:     scope,
		Clock:     r.config.Clock,
		Logger:    r.config.Logger.With(slog.String("engine", selected.Name()), slog.String("step_id", spec.StepID.String())),
	}
	engineResult, execErr := selected.Execute(execCtx, request)
	finished := r.config.Clock.Now()
	if execErr != nil {
		class := domain.ErrExecutorError
		retryable := true
		switch {
		case errors.Is(execErr, context.DeadlineExceeded), errors.Is(execCtx.Err(), context.DeadlineExceeded):
			class = domain.ErrTimeout
			retryable = false
		case errors.Is(execErr, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
			class = domain.ErrCancelled
			retryable = false
		case errors.Is(execErr, domain.ErrUnsupported):
			class = domain.ErrEngineUnsupported
			retryable = false
		}
		return failedResult(class, execErr.Error(), retryable, finished), nil
	}
	result = domain.StepResult{
		Outcome:         engineResult.Outcome,
		ErrorClass:      engineResult.ErrorClass,
		Message:         engineResult.Message,
		Retryable:       engineResult.Retryable,
		Evidence:        engineResult.Evidence,
		Counterexamples: engineResult.Counterexamples,
		Artifacts:       engineResult.Artifacts,
		Metrics:         engineResult.Metrics,
		Outputs:         engineResult.Outputs,
		FinishedAt:      finished,
	}
	result.Metrics.ExecutionMS = finished.Sub(started).Milliseconds()
	if err := r.finalizeResult(spec, selected, &result); err != nil {
		return failedResult(domain.ErrValidationFailed, "invalid engine result: "+err.Error(), false, finished), nil
	}
	return result, nil
}

func (r *Runner) finalizeResult(spec domain.StepSpec, selected engine.Engine, result *domain.StepResult) error {
	if !result.Outcome.Valid() {
		return fmt.Errorf("engine returned an invalid outcome")
	}
	if len(result.Artifacts) > r.config.MaxArtifacts {
		return fmt.Errorf("engine produced %d artifacts, limit is %d", len(result.Artifacts), r.config.MaxArtifacts)
	}
	claimID := domain.ClaimID(stepopts.Get(spec.Options, stepopts.ClaimID))
	now := r.config.Clock.Now()
	for i := range result.Evidence {
		entry := &result.Evidence[i]
		if entry.ID == "" {
			entry.ID = domain.NewEvidenceID()
		}
		entry.TenantID = spec.TenantID
		entry.RunID = spec.RunID
		entry.StepID = spec.StepID
		if entry.ClaimID == "" {
			entry.ClaimID = claimID
		}
		if entry.SchemaVersion == "" {
			entry.SchemaVersion = domain.EvidenceSchemaV1
		}
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = now
		}
		if entry.Engine.Name == "" {
			entry.Engine.Name = selected.Name()
			entry.Engine.Version = selected.Version()
		}
		if entry.Environment.Arch == "" {
			entry.Environment.Arch = runtime.GOARCH
		}
		if entry.Environment.SandboxProfile == "" {
			entry.Environment.SandboxProfile = string(spec.Requirements.Isolation)
		}
		if entry.Environment.Region == "" {
			entry.Environment.Region = spec.Requirements.Region
		}
		if err := entry.Validate(); err != nil {
			return fmt.Errorf("evidence %s: %w", entry.ID, err)
		}
	}
	for i := range result.Counterexamples {
		entry := &result.Counterexamples[i]
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = now
		}
		if entry.TenantID == "" {
			entry.TenantID = spec.TenantID
		}
		if entry.RunID == "" {
			entry.RunID = spec.RunID
		}
		if entry.ClaimID == "" {
			entry.ClaimID = claimID
		}
		if err := entry.Validate(); err != nil {
			return fmt.Errorf("counterexample %s: %w", entry.CaseID, err)
		}
	}
	digest, err := evidence.CanonicalDigest(struct {
		Evidence        []domain.Evidence       `json:"evidence"`
		Counterexamples []domain.Counterexample `json:"counterexamples"`
		Outcome         domain.StepOutcome      `json:"outcome"`
		Outputs         map[string]string       `json:"outputs"`
	}{
		Evidence:        result.Evidence,
		Counterexamples: result.Counterexamples,
		Outcome:         result.Outcome,
		Outputs:         result.Outputs,
	})
	if err != nil {
		return err
	}
	result.ResultDigest = digest
	if result.Metrics.ExecutionMS == 0 {
		result.Metrics.ExecutionMS = 1
	}
	return result.Validate()
}

func (r *Runner) prepareWorkspace(spec domain.StepSpec) (string, func(), error) {
	base := filepath.Join(r.config.WorkDir, "patchproof-work")
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", nil, fmt.Errorf("create work root: %w", err)
	}
	name := fmt.Sprintf("%s-%s", sanitize(spec.StepID.String()), sanitize(spec.AttemptID.String()))
	workspace := filepath.Join(base, name)
	if err := os.RemoveAll(workspace); err != nil {
		return "", nil, fmt.Errorf("reset workspace: %w", err)
	}
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		return "", nil, fmt.Errorf("create workspace: %w", err)
	}
	return workspace, func() {
		if err := os.RemoveAll(workspace); err != nil {
			r.config.Logger.Warn("workspace cleanup failed", slog.String("path", workspace), slog.String("error", err.Error()))
		}
	}, nil
}

func (r *Runner) stageInputs(ctx context.Context, spec domain.StepSpec, workspace string, scope ports.SecurityScope) (map[string]engine.StagedInput, error) {
	staged := make(map[string]engine.StagedInput, len(spec.NamedInputs))
	inputDir := filepath.Join(workspace, "inputs")
	snapshotDir := filepath.Join(workspace, "snapshots")
	if err := os.MkdirAll(inputDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(snapshotDir, 0o750); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(spec.NamedInputs))
	for name := range spec.NamedInputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		binding := spec.NamedInputs[name]
		if err := binding.Validate(); err != nil {
			return nil, fmt.Errorf("input %s: %w", name, err)
		}
		switch binding.Kind {
		case "file":
			target := filepath.Join(inputDir, sanitize(name))
			if err := r.download(ctx, scope, binding.Ref, target, false); err != nil {
				return nil, fmt.Errorf("input %s: %w", name, err)
			}
			staged[name] = engine.StagedInput{Name: name, Path: target, Ref: binding.Ref, Kind: binding.Kind}
		case "tar.gz", "tar.zst":
			if binding.Kind == "tar.zst" {
				return nil, fmt.Errorf("input %s: tar.zst inputs are not supported by this worker build", name)
			}
			target := filepath.Join(snapshotDir, sanitize(name))
			if err := os.MkdirAll(target, 0o750); err != nil {
				return nil, err
			}
			if err := r.download(ctx, scope, binding.Ref, target+".tar.gz", true); err != nil {
				return nil, fmt.Errorf("input %s: %w", name, err)
			}
			if err := extractTarGz(target+".tar.gz", target, r.config.MaxExtractBytes, r.config.MaxFiles); err != nil {
				return nil, fmt.Errorf("extract %s: %w", name, err)
			}
			_ = os.Remove(target + ".tar.gz")
			staged[name] = engine.StagedInput{Name: name, Path: target, Ref: binding.Ref, Kind: "tar.gz"}
		}
	}
	return staged, nil
}

func (r *Runner) download(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef, target string, allowLarge bool) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if !allowLarge && ref.SizeBytes > 64<<20 {
		return fmt.Errorf("file input exceeds 64 MiB")
	}
	reader, err := r.deps.Store.Open(ctx, scope, ref)
	if err != nil {
		return err
	}
	defer reader.Close()
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(reader, r.config.MaxExtractBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > r.config.MaxExtractBytes {
		return fmt.Errorf("input exceeds size limit")
	}
	if hex.EncodeToString(hasher.Sum(nil)) != ref.Hex() {
		return fmt.Errorf("artifact digest mismatch")
	}
	return nil
}

func validateSpec(spec domain.StepSpec) error {
	if spec.RunID == "" || spec.StepID == "" || spec.AttemptID == "" {
		return fmt.Errorf("step spec is missing identifiers")
	}
	if spec.TenantID.Validate() != nil {
		return fmt.Errorf("step spec has an invalid tenant")
	}
	if spec.Generation <= 0 {
		return fmt.Errorf("step spec has an invalid generation")
	}
	if !spec.Kind.Valid() {
		return fmt.Errorf("step spec has an unknown kind")
	}
	if err := spec.Requirements.Validate(); err != nil {
		return err
	}
	for _, input := range spec.NamedInputs {
		if err := input.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func ScopeFor(spec domain.StepSpec) ports.SecurityScope {
	region := spec.Requirements.Region
	if region == "" {
		region = "local"
	}
	return ports.SecurityScope{TenantID: spec.TenantID, TrustDomain: "run", Region: region}
}

func effectiveDeadline(spec domain.StepSpec, now time.Time) time.Time {
	if !spec.Deadline.IsZero() {
		return spec.Deadline
	}
	deadline := now.Add(spec.Requirements.Deadline())
	if spec.Requirements.Deadline() <= 0 {
		deadline = now.Add(10 * time.Minute)
	}
	return deadline
}

func failedResult(class domain.ErrorClass, message string, retryable bool, at time.Time) domain.StepResult {
	return domain.StepResult{
		Outcome:    domain.OutcomeFailed,
		ErrorClass: class,
		Message:    message,
		Retryable:  retryable,
		FinishedAt: at,
	}
}

func sanitize(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := builder.String()
	if len(out) > 96 {
		out = out[:96]
	}
	if out == "" {
		return "step"
	}
	return out
}

func extractTarGz(archivePath, destination string, maxBytes int64, maxFiles int) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	root, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	var total int64
	files := 0
	seen := map[string]struct{}{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		files++
		if files > maxFiles {
			return fmt.Errorf("archive contains too many entries")
		}
		name := filepath.Clean(header.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("archive entry escapes the workspace")
		}
		target := filepath.Join(root, name)
		relative, err := filepath.Rel(root, target)
		if err != nil || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("archive entry escapes the workspace")
		}
		if _, duplicate := seen[relative]; duplicate {
			return fmt.Errorf("duplicate archive entry %q", relative)
		}
		seen[relative] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maxBytes {
				return fmt.Errorf("archive entry %q exceeds size limit", relative)
			}
			total += header.Size
			if total > maxBytes {
				return fmt.Errorf("archive exceeds total size limit")
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(out, reader, header.Size); err != nil {
				out.Close()
				return fmt.Errorf("extract %q: %w", relative, err)
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive contains a link entry %q, which is not allowed", relative)
		default:
			continue
		}
	}
	return nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
