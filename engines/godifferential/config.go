package godifferential

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/contract"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

const (
	defaultMaxCases          = 20000
	defaultFuzzSeconds       = 30
	defaultMinCases          = 100
	defaultBatchSize         = 512
	defaultMaxCounterexample = 3
	defaultBatchTimeout      = 5 * time.Second
	defaultFloatEpsilon      = 1e-9
	maxFuzzSeconds           = 600
	maxBatchSize             = 4096
	maxCasesLimit            = 1_000_000
	maxCounterexamplesLimit  = 64
	maxShrinkSteps           = 200
	maxShrinkDuration        = 2 * time.Minute
)

var claimIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)

// options captures the step option knobs understood by this engine. Unknown
// options are ignored so the planner can add new knobs without breaking older
// engine versions.
type options struct {
	claimID            domain.ClaimID
	seed               int64
	maxCases           int
	duration           time.Duration
	minCases           int64
	batchSize          int
	batchTimeout       time.Duration
	maxCounterexamples int
	maxShrinkSteps     int
	floatEpsilon       float64
	proxy              string
	normalize          []string
	ignoreFields       []string
	inputDomain        string
}

func parseOptions(document contract.Document, raw map[string]string) (options, error) {
	opts := options{
		seed:               document.Spec.Domain.Seed,
		maxCases:           defaultMaxCases,
		duration:           defaultFuzzSeconds * time.Second,
		minCases:           defaultMinCases,
		batchSize:          defaultBatchSize,
		batchTimeout:       defaultBatchTimeout,
		maxCounterexamples: defaultMaxCounterexample,
		maxShrinkSteps:     maxShrinkSteps,
		floatEpsilon:       defaultFloatEpsilon,
		proxy:              stepopts.Get(raw, stepopts.Goproxy),
		normalize:          append([]string(nil), document.Spec.Oracle.Normalize...),
		ignoreFields:       append([]string(nil), document.Spec.Oracle.IgnoreFields...),
		inputDomain:        document.Spec.Domain.Description,
	}
	if value := stepopts.Get(raw, "fuzz.seed"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return options{}, fmt.Errorf("option fuzz.seed must be an integer: %w", err)
		}
		opts.seed = parsed
	}
	if value := stepopts.Get(raw, "fuzz.max_cases"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return options{}, fmt.Errorf("option fuzz.max_cases must be a positive integer")
		}
		opts.maxCases = minInt(opts.maxCases, parsed)
	}
	if document.Spec.Budget.MaxCases > 0 {
		opts.maxCases = minInt(opts.maxCases, document.Spec.Budget.MaxCases)
	}
	if opts.maxCases > maxCasesLimit {
		opts.maxCases = maxCasesLimit
	}

	if value := stepopts.Get(raw, "fuzz.seconds"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return options{}, fmt.Errorf("option fuzz.seconds must be a positive integer")
		}
		opts.duration = minDuration(opts.duration, time.Duration(parsed)*time.Second)
	}
	if document.Spec.Budget.MaxDurationSecs > 0 {
		opts.duration = minDuration(opts.duration, time.Duration(document.Spec.Budget.MaxDurationSecs)*time.Second)
	}
	if value := stepopts.Get(raw, "timeout.seconds"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return options{}, fmt.Errorf("option timeout.seconds must be a positive integer")
		}
		opts.duration = minDuration(opts.duration, time.Duration(parsed)*time.Second)
	}
	if opts.duration > maxFuzzSeconds*time.Second {
		opts.duration = maxFuzzSeconds * time.Second
	}

	if value := stepopts.Get(raw, "differential.min_cases"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			return options{}, fmt.Errorf("option differential.min_cases must be a positive integer")
		}
		opts.minCases = parsed
	}
	if opts.minCases < 1 {
		opts.minCases = 1
	}

	if value := stepopts.Get(raw, "differential.batch_size"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return options{}, fmt.Errorf("option differential.batch_size must be a positive integer")
		}
		opts.batchSize = parsed
	}
	if opts.batchSize > maxBatchSize {
		opts.batchSize = maxBatchSize
	}
	if opts.batchSize > opts.maxCases {
		opts.batchSize = maxInt(1, opts.maxCases)
	}

	if document.Spec.TimeoutMillis > 0 {
		opts.batchTimeout = time.Duration(document.Spec.TimeoutMillis) * time.Millisecond
	}
	if value := stepopts.Get(raw, "differential.batch_timeout_ms"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return options{}, fmt.Errorf("option differential.batch_timeout_ms must be a positive integer")
		}
		opts.batchTimeout = time.Duration(parsed) * time.Millisecond
	}
	if opts.batchTimeout < time.Second {
		opts.batchTimeout = time.Second
	}
	if opts.batchTimeout > 2*time.Minute {
		opts.batchTimeout = 2 * time.Minute
	}

	if value := stepopts.Get(raw, "differential.max_counterexamples"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return options{}, fmt.Errorf("option differential.max_counterexamples must be a positive integer")
		}
		opts.maxCounterexamples = parsed
	}
	if opts.maxCounterexamples > maxCounterexamplesLimit {
		opts.maxCounterexamples = maxCounterexamplesLimit
	}

	if value := stepopts.Get(raw, "oracle.float_epsilon"); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
			return options{}, fmt.Errorf("option oracle.float_epsilon must be a non-negative finite number")
		}
		opts.floatEpsilon = parsed
	}

	if value := stepopts.Get(raw, stepopts.InputDomain); value != "" {
		opts.inputDomain = value
	}

	claim := stepopts.Get(raw, stepopts.ClaimID)
	if claim == "" {
		claim = document.Metadata.Name
	}
	if !claimIDPattern.MatchString(claim) {
		return options{}, fmt.Errorf("claim id %q is invalid; set option %s to a stable identifier", claim, stepopts.ClaimID)
	}
	opts.claimID = domain.ClaimID(claim)
	return opts, nil
}

// config is the fully validated execution plan for one engine invocation.
type config struct {
	workspace        string
	baseDir          string
	headDir          string
	baseModule       string
	headModule       string
	contract         contract.Document
	symbol           contract.SymbolRef
	contractRef      *domain.CASRef
	contractFallback bool
	opts             options
	oracle           oracleConfig

	env        harness.Environment
	baseBinary string
	headBinary string

	store   ports.ArtifactStore
	scope   ports.SecurityScope
	now     time.Time
	logger  *slog.Logger
	tenant  domain.TenantID
	runID   domain.RunID
	cleanup []func()
}

func newConfig(ctx context.Context, request engine.Request) (*config, *engine.Result) {
	if strings.TrimSpace(request.Workspace) == "" {
		return nil, failure(domain.ErrValidationFailed, "step workspace is required", false, nil)
	}
	if request.Store == nil {
		return nil, failure(domain.ErrValidationFailed, "artifact store is required", false, nil)
	}
	workspace, err := filepath.Abs(request.Workspace)
	if err != nil {
		return nil, failure(domain.ErrValidationFailed, "resolve workspace: "+err.Error(), false, nil)
	}
	baseDir, ok := stagedPath(request, "base", stepopts.BaseSnapshot)
	if !ok {
		return nil, failure(domain.ErrValidationFailed, "staged input base is required", false, nil)
	}
	headDir, ok := stagedPath(request, "head", stepopts.HeadSnapshot)
	if !ok {
		return nil, failure(domain.ErrValidationFailed, "staged input head is required", false, nil)
	}
	for name, dir := range map[string]string{"base": baseDir, "head": headDir} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return nil, failure(domain.ErrValidationFailed, fmt.Sprintf("staged %s snapshot %s is not a directory", name, dir), false, nil)
		}
	}

	contractPath, err := resolveContractPath(request, baseDir, headDir, workspace)
	if err != nil {
		return nil, failure(domain.ErrValidationFailed, err.Error(), false, nil)
	}
	document, fallback, err := loadContract(contractPath)
	if err != nil {
		return nil, failure(domain.ErrEngineUnsupported, "load contract: "+err.Error(), false, nil)
	}
	symbol, err := document.SymbolRef()
	if err != nil {
		return nil, failure(domain.ErrEngineUnsupported, err.Error(), false, nil)
	}
	if symbol.Receiver != "" {
		return nil, failure(domain.ErrEngineUnsupported, fmt.Sprintf("symbol %s is a method; only package-level functions are supported", document.Spec.Symbol), false, nil)
	}
	opts, err := parseOptions(document, request.Spec.Options)
	if err != nil {
		return nil, failure(domain.ErrValidationFailed, err.Error(), false, nil)
	}
	oracle, err := newOracleConfig(document, opts.floatEpsilon)
	if err != nil {
		return nil, failure(domain.ErrEngineUnsupported, err.Error(), false, nil)
	}

	environment, err := harness.NewEnvironment(ctx, workspace, opts.proxy)
	if err != nil {
		return nil, failure(domain.ErrEngineUnsupported, err.Error(), false, nil)
	}

	baseModule, err := harness.ModulePath(baseDir)
	if err != nil {
		return nil, failure(domain.ErrEngineUnsupported, "base snapshot: "+err.Error(), false, nil)
	}
	headModule, err := harness.ModulePath(headDir)
	if err != nil {
		return nil, failure(domain.ErrEngineUnsupported, "head snapshot: "+err.Error(), false, nil)
	}
	if baseModule != headModule {
		return nil, failure(domain.ErrEngineUnsupported, fmt.Sprintf("base module %q and head module %q differ; differential instrumentation requires identical module paths", baseModule, headModule), false, nil)
	}

	now := time.Now().UTC()
	if request.Clock != nil {
		now = request.Clock.Now().UTC()
	}
	cfg := &config{
		workspace:        workspace,
		baseDir:          baseDir,
		headDir:          headDir,
		baseModule:       baseModule,
		headModule:       headModule,
		contract:         document,
		contractFallback: fallback,
		symbol:           symbol,
		opts:             opts,
		oracle:           oracle,
		env:              environment,
		baseBinary:       filepath.Join(workspace, "bin", "harness-base"),
		headBinary:       filepath.Join(workspace, "bin", "harness-head"),
		now:              now,
		store:            request.Store,
		tenant:           request.Spec.TenantID,
		runID:            request.Spec.RunID,
		scope:            request.Scope,
		logger:           request.Logger,
	}
	if request.Staged != nil {
		if staged, ok := request.Staged["contract"]; ok && staged.Ref.Digest != "" {
			ref := staged.Ref
			cfg.contractRef = &ref
		}
	}
	if request.Spec.Deadline != (time.Time{}) {
		remaining := time.Until(request.Spec.Deadline) - time.Second
		if remaining < time.Second {
			return nil, failure(domain.ErrValidationFailed, "step deadline has already elapsed", false, nil)
		}
		if remaining < cfg.opts.duration {
			cfg.opts.duration = remaining
		}
	}
	return cfg, nil
}

func stagedPath(request engine.Request, name, optionKey string) (string, bool) {
	if request.Staged != nil {
		if staged, ok := request.Staged[name]; ok && strings.TrimSpace(staged.Path) != "" {
			absolute, err := filepath.Abs(staged.Path)
			if err != nil {
				return "", false
			}
			return absolute, true
		}
	}
	if value := stepopts.Get(request.Spec.Options, optionKey); value != "" {
		absolute, err := filepath.Abs(value)
		if err != nil {
			return "", false
		}
		return absolute, true
	}
	return "", false
}

// loadContract parses a contract through pkg/contract first. The platform
// parser cannot currently decode YAML seed arguments into json.RawMessage
// (scalars are rejected), so a lossless YAML -> JSON re-decode through the same
// contract.Document and Validate rules is used as a compatibility fallback. The
// fallback never accepts a document that pkg/contract's validation rejects.
func loadContract(path string) (contract.Document, bool, error) {
	document, err := contract.LoadFile(path)
	if err == nil {
		return document, false, nil
	}
	info, statErr := os.Stat(path)
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return contract.Document{}, false, err
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		return contract.Document{}, false, err
	}
	var generic any
	if yamlErr := yaml.Unmarshal(raw, &generic); yamlErr != nil {
		return contract.Document{}, false, err
	}
	encoded, jsonErr := json.Marshal(generic)
	if jsonErr != nil {
		return contract.Document{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var fallback contract.Document
	if decodeErr := decoder.Decode(&fallback); decodeErr != nil {
		return contract.Document{}, false, err
	}
	if validateErr := fallback.Validate(); validateErr != nil {
		return contract.Document{}, false, err
	}
	return fallback, true, nil
}

// resolveContractPath enforces that the contract file lives inside one of the
// staged snapshots or the step workspace.
func resolveContractPath(request engine.Request, baseDir, headDir, workspace string) (string, error) {
	roots := []string{baseDir, headDir, workspace}
	candidates := make([]string, 0, 2)
	if request.Staged != nil {
		if staged, ok := request.Staged["contract"]; ok && strings.TrimSpace(staged.Path) != "" {
			absolute, err := filepath.Abs(staged.Path)
			if err != nil {
				return "", fmt.Errorf("resolve staged contract: %w", err)
			}
			candidates = append(candidates, absolute)
		}
	}
	if value := stepopts.Get(request.Spec.Options, stepopts.ContractRef); value != "" {
		// contract.ref is resolved relative to the head snapshot root; the
		// resolver rejects absolute paths and parent-directory escapes.
		path, err := contract.ResolvePath(headDir, value)
		if err == nil {
			candidates = append(candidates, path)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("contract input is missing; stage a contract file or set option %s", stepopts.ContractRef)
	}
	for _, candidate := range candidates {
		if !withinAny(candidate, roots) {
			continue
		}
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("contract file is outside the staged snapshots and workspace or does not exist")
}

func withinAny(path string, roots []string) bool {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	resolved := absolute
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		resolved = evaluated
	}
	for _, root := range roots {
		rootAbsolute, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rootResolved := rootAbsolute
		if evaluated, err := filepath.EvalSymlinks(rootAbsolute); err == nil {
			rootResolved = evaluated
		}
		relative, err := filepath.Rel(rootResolved, resolved)
		if err != nil {
			continue
		}
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// failure builds a classified engine result.
func failure(class domain.ErrorClass, message string, retryable bool, artifacts []domain.CASRef) *engine.Result {
	outcome := domain.OutcomeFailed
	if retryable {
		outcome = domain.OutcomeRetryable
	}
	return &engine.Result{
		Outcome:    outcome,
		ErrorClass: class,
		Message:    message,
		Retryable:  retryable,
		Artifacts:  artifacts,
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
