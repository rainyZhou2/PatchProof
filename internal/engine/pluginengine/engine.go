// Package pluginengine adapts an out-of-process stdio plugin to the
// engine.Engine interface.
//
// One plugin process is started per Execute call. Staged inputs are handed to
// the plugin as sandbox-local paths, and every output file the plugin names is
// uploaded by the host into tenant-scoped CAS; the plugin never needs
// credentials for CAS. Evidence and counterexample drafts are completed and
// validated by the host before they are returned to the worker.
package pluginengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/plugin"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

const (
	// Protocol is the wire protocol identifier recorded in evidence.
	Protocol = "stdio"

	defaultTimeout                = 10 * time.Minute
	capabilitiesTimeout           = 15 * time.Second
	maxOutputFiles                = 256
	maxEvidenceDrafts             = 512
	maxCounterexampleDrafts       = 512
	maxOutputBytes          int64 = 4 << 30
)

// Config describes one external plugin. Command is executed directly; no shell
// is involved. Env entries are appended to the worker's environment, so the
// plugin inherits the toolchain environment while still being able to override
// individual variables.
type Config struct {
	Name          string
	Version       string
	Command       []string
	Env           []string
	Languages     []string
	Kinds         []domain.StepKind
	MaxFrameBytes int
	Timeout       time.Duration
	// Dialer overrides process startup. It is used by tests and by deployments
	// that connect to a plugin over a pre-established transport. The returned
	// client is owned by Execute and closed when the step finishes.
	Dialer func(context.Context) (*plugin.Client, error)
}

// Engine is an engine.Engine backed by an external stdio plugin.
type Engine struct {
	cfg Config
}

var _ engine.Engine = (*Engine)(nil)

// New validates cfg and returns a ready engine. Names, commands, languages and
// kinds are required because the registry relies on them for routing.
func New(cfg Config) (*Engine, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if cfg.Name == "" {
		return nil, errors.New("pluginengine: name is required")
	}
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("pluginengine[%s]: command is required", cfg.Name)
	}
	command := make([]string, len(cfg.Command))
	for i, arg := range cfg.Command {
		if strings.TrimSpace(arg) == "" {
			return nil, fmt.Errorf("pluginengine[%s]: command argument %d is empty", cfg.Name, i)
		}
		if strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("pluginengine[%s]: command argument %d contains NUL", cfg.Name, i)
		}
		command[i] = arg
	}
	cfg.Command = command

	if len(cfg.Languages) == 0 {
		return nil, fmt.Errorf("pluginengine[%s]: at least one language is required", cfg.Name)
	}
	languages := make([]string, 0, len(cfg.Languages))
	seenLanguages := make(map[string]struct{}, len(cfg.Languages))
	for _, language := range cfg.Languages {
		trimmed := strings.TrimSpace(language)
		if trimmed == "" {
			return nil, fmt.Errorf("pluginengine[%s]: language must not be empty", cfg.Name)
		}
		if strings.ContainsRune(trimmed, 0) {
			return nil, fmt.Errorf("pluginengine[%s]: language %q contains NUL", cfg.Name, trimmed)
		}
		key := strings.ToLower(trimmed)
		if _, duplicate := seenLanguages[key]; duplicate {
			continue
		}
		seenLanguages[key] = struct{}{}
		languages = append(languages, trimmed)
	}
	cfg.Languages = languages

	if len(cfg.Kinds) == 0 {
		return nil, fmt.Errorf("pluginengine[%s]: at least one step kind is required", cfg.Name)
	}
	kinds := make([]domain.StepKind, 0, len(cfg.Kinds))
	seenKinds := make(map[domain.StepKind]struct{}, len(cfg.Kinds))
	for _, kind := range cfg.Kinds {
		if !kind.Valid() {
			return nil, fmt.Errorf("pluginengine[%s]: unknown step kind %q", cfg.Name, kind)
		}
		if _, duplicate := seenKinds[kind]; duplicate {
			continue
		}
		seenKinds[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	cfg.Kinds = kinds

	if cfg.MaxFrameBytes < 0 {
		return nil, fmt.Errorf("pluginengine[%s]: max frame bytes must not be negative", cfg.Name)
	}
	if cfg.MaxFrameBytes == 0 {
		cfg.MaxFrameBytes = plugin.DefaultMaxFrameBytes
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("pluginengine[%s]: timeout must not be negative", cfg.Name)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}

	env := make([]string, len(cfg.Env))
	for i, entry := range cfg.Env {
		if strings.ContainsRune(entry, 0) {
			return nil, fmt.Errorf("pluginengine[%s]: environment entry %d contains NUL", cfg.Name, i)
		}
		if !strings.Contains(entry, "=") || strings.HasPrefix(entry, "=") {
			return nil, fmt.Errorf("pluginengine[%s]: environment entry %q must be KEY=VALUE", cfg.Name, entry)
		}
		env[i] = entry
	}
	cfg.Env = env

	return &Engine{cfg: cfg}, nil
}

// Name returns the registry name of the plugin.
func (e *Engine) Name() string { return e.cfg.Name }

// Version returns the configured plugin version.
func (e *Engine) Version() string { return e.cfg.Version }

// Kinds returns the step kinds the plugin handles.
func (e *Engine) Kinds() []domain.StepKind {
	return append([]domain.StepKind(nil), e.cfg.Kinds...)
}

// Languages returns the languages the plugin handles.
func (e *Engine) Languages() []string {
	return append([]string(nil), e.cfg.Languages...)
}

// Execute runs one step through a fresh plugin process.
func (e *Engine) Execute(ctx context.Context, request engine.Request) (engine.Result, error) {
	if err := e.validateRequest(request); err != nil {
		return engine.Result{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()
	if err := runCtx.Err(); err != nil {
		return engine.Result{}, fmt.Errorf("pluginengine[%s]: %w", e.cfg.Name, err)
	}

	outputDir, err := prepareOutputDir(request.Workspace, request.Spec.StepID, request.Spec.AttemptID)
	if err != nil {
		return engine.Result{}, fmt.Errorf("pluginengine[%s]: %w", e.cfg.Name, err)
	}

	client, err := e.dial(runCtx, request.Workspace)
	if err != nil {
		return engine.Result{}, fmt.Errorf("pluginengine[%s]: dial: %w", e.cfg.Name, err)
	}
	defer func() { _ = client.Close() }()

	capabilities, err := e.capabilities(runCtx, client)
	if err != nil {
		return engine.Result{}, fmt.Errorf("pluginengine[%s]: %w", e.cfg.Name, err)
	}
	language := resolveLanguage(request.Spec)
	if err := verifyCapabilities(capabilities, request.Spec.Kind, language); err != nil {
		return engine.Result{}, fmt.Errorf("pluginengine[%s]: %w", e.cfg.Name, err)
	}

	deadline, _ := runCtx.Deadline()
	if !request.Spec.Deadline.IsZero() && (deadline.IsZero() || request.Spec.Deadline.Before(deadline)) {
		deadline = request.Spec.Deadline
	}

	response, err := client.Execute(runCtx, e.buildStepRequest(request, outputDir, language, deadline))
	if err != nil {
		return engine.Result{}, fmt.Errorf("pluginengine[%s]: execute: %w", e.cfg.Name, err)
	}

	result, err := e.mapResponse(runCtx, request, response, outputDir, e.descriptorVersion(capabilities))
	if err != nil {
		return engine.Result{}, fmt.Errorf("pluginengine[%s]: %w", e.cfg.Name, err)
	}
	return result, nil
}

func (e *Engine) validateRequest(request engine.Request) error {
	spec := request.Spec
	if spec.RunID == "" || spec.StepID == "" || spec.AttemptID == "" {
		return errors.New("pluginengine: step identifiers are required")
	}
	if spec.Generation <= 0 {
		return errors.New("pluginengine: fencing generation must be positive")
	}
	if !spec.Kind.Valid() {
		return fmt.Errorf("pluginengine: unknown step kind %q", spec.Kind)
	}
	if !containsKind(e.cfg.Kinds, spec.Kind) {
		return fmt.Errorf("%w: plugin %s does not handle kind %s", domain.ErrUnsupported, e.cfg.Name, spec.Kind)
	}
	language := resolveLanguage(spec)
	if language != "" && !containsFold(e.cfg.Languages, language) {
		return fmt.Errorf("%w: plugin %s does not handle language %s", domain.ErrUnsupported, e.cfg.Name, language)
	}
	if strings.TrimSpace(request.Workspace) == "" {
		return errors.New("pluginengine: workspace is required")
	}
	if request.Store == nil {
		return errors.New("pluginengine: artifact store is required")
	}
	for name, input := range request.Staged {
		if strings.TrimSpace(name) == "" {
			return errors.New("pluginengine: staged input name must not be empty")
		}
		if err := input.Ref.Validate(); err != nil {
			return fmt.Errorf("pluginengine: staged input %q: %w", name, err)
		}
	}
	return nil
}

func (e *Engine) dial(ctx context.Context, workspace string) (*plugin.Client, error) {
	if e.cfg.Dialer != nil {
		client, err := e.cfg.Dialer(ctx)
		if err != nil {
			return nil, err
		}
		if client == nil {
			return nil, errors.New("dialer returned a nil client")
		}
		return client, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Inherit the worker toolchain environment, then apply plugin overrides.
	env := append(os.Environ(), e.cfg.Env...)
	return plugin.Dial(e.cfg.Command, plugin.CommandConfig{
		Env:           env,
		Dir:           workspace,
		MaxFrameBytes: e.cfg.MaxFrameBytes,
	})
}

func (e *Engine) capabilities(ctx context.Context, client *plugin.Client) (plugin.Capabilities, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, capabilitiesTimeout)
	defer cancel()
	capabilities, err := client.Capabilities(handshakeCtx)
	if err != nil {
		return plugin.Capabilities{}, fmt.Errorf("capabilities: %w", err)
	}
	return capabilities, nil
}

func (e *Engine) descriptorVersion(capabilities plugin.Capabilities) string {
	if version := strings.TrimSpace(capabilities.Version); version != "" {
		return version
	}
	if version := strings.TrimSpace(e.cfg.Version); version != "" {
		return version
	}
	return "unknown"
}

func (e *Engine) buildStepRequest(request engine.Request, outputDir, language string, deadline time.Time) plugin.StepRequest {
	staged := make(map[string]plugin.StagedInput, len(request.Staged))
	for name, input := range request.Staged {
		staged[name] = plugin.StagedInput{
			Ref: plugin.CASRef{
				Algorithm:   input.Ref.Algorithm,
				Digest:      input.Ref.Digest,
				SizeBytes:   input.Ref.SizeBytes,
				TenantScope: input.Ref.TenantScope,
			},
			Path: input.Path,
			Kind: input.Kind,
		}
	}
	var options map[string]string
	if len(request.Spec.Options) > 0 {
		options = make(map[string]string, len(request.Spec.Options))
		for key, value := range request.Spec.Options {
			options[key] = value
		}
	}
	return plugin.StepRequest{
		RunID:             request.Spec.RunID.String(),
		StepID:            request.Spec.StepID.String(),
		AttemptID:         request.Spec.AttemptID.String(),
		FencingGeneration: uint64(request.Spec.Generation),
		PolicyDigest:      request.Spec.PolicyDigest,
		StepKind:          string(request.Spec.Kind),
		Language:          language,
		Engine:            e.cfg.Name,
		Workspace:         request.Workspace,
		OutputDir:         outputDir,
		StagedInputs:      staged,
		Options:           options,
		Deadline:          deadline,
	}
}

func resolveLanguage(spec domain.StepSpec) string {
	if language := strings.TrimSpace(spec.Language); language != "" {
		return language
	}
	return strings.TrimSpace(stepopts.Get(spec.Options, stepopts.Language))
}

func verifyCapabilities(capabilities plugin.Capabilities, kind domain.StepKind, language string) error {
	if len(capabilities.StepKinds) > 0 && !containsExact(capabilities.StepKinds, string(kind)) {
		return fmt.Errorf("%w: plugin does not advertise step kind %s", domain.ErrUnsupported, kind)
	}
	if language != "" && len(capabilities.Languages) > 0 && !containsFold(capabilities.Languages, language) {
		return fmt.Errorf("%w: plugin does not advertise language %s", domain.ErrUnsupported, language)
	}
	return nil
}

func containsKind(kinds []domain.StepKind, target domain.StepKind) bool {
	for _, kind := range kinds {
		if kind == target {
			return true
		}
	}
	return false
}

func containsExact(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
