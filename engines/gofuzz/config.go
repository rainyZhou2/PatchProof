package gofuzz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	defaultFuzzSeconds = 10
	maxFuzzSeconds     = 600
	fuzzTimeoutGrace   = 90 * time.Second
	maxCorpusFiles     = 16
	maxCorpusFileBytes = 1 << 20
)

var claimIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)

type config struct {
	workspace  string
	headDir    string
	headModule string
	contract   contract.Document
	symbol     contract.SymbolRef
	claimID    domain.ClaimID
	seconds    int
	proxy      string

	env               harness.Environment
	toolErr           error
	fuzzCacheFallback bool

	store   ports.ArtifactStore
	scope   ports.SecurityScope
	tenant  domain.TenantID
	runID   domain.RunID
	now     time.Time
	logger  *slog.Logger
	cleanup []func()

	contractRef      *domain.CASRef
	contractFallback bool
	seedCount        int64
	seeds            [][]byte
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
	headDir, ok := stagedPath(request, "head", stepopts.HeadSnapshot)
	if !ok {
		return nil, failure(domain.ErrValidationFailed, "staged input head is required", false, nil)
	}
	info, statErr := os.Stat(headDir)
	if statErr != nil || !info.IsDir() {
		return nil, failure(domain.ErrValidationFailed, fmt.Sprintf("staged head snapshot %s is not a directory", headDir), false, nil)
	}

	contractPath, err := resolveContractPath(request, headDir, workspace)
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
	claim := stepopts.Get(request.Spec.Options, stepopts.ClaimID)
	if claim == "" {
		claim = document.Metadata.Name
	}
	if !claimIDPattern.MatchString(claim) {
		return nil, failure(domain.ErrValidationFailed, fmt.Sprintf("claim id %q is invalid", claim), false, nil)
	}

	seconds := defaultFuzzSeconds
	if value := stepopts.Get(request.Spec.Options, "fuzz.seconds"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed <= 0 {
			return nil, failure(domain.ErrValidationFailed, "option fuzz.seconds must be a positive integer", false, nil)
		}
		seconds = minInt(seconds, parsed)
	}
	if document.Spec.Budget.MaxDurationSecs > 0 {
		seconds = minInt(seconds, document.Spec.Budget.MaxDurationSecs)
	}
	if value := stepopts.Get(request.Spec.Options, "timeout.seconds"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed <= 0 {
			return nil, failure(domain.ErrValidationFailed, "option timeout.seconds must be a positive integer", false, nil)
		}
		seconds = minInt(seconds, parsed)
	}
	if seconds > maxFuzzSeconds {
		seconds = maxFuzzSeconds
	}
	if seconds < 1 {
		seconds = 1
	}

	module, err := harness.ModulePath(headDir)
	if err != nil {
		return nil, failure(domain.ErrEngineUnsupported, "head snapshot: "+err.Error(), false, nil)
	}
	if _, err := harness.PackageDir(headDir, module, symbol.PackagePath); err != nil {
		return nil, failure(domain.ErrEngineUnsupported, "head snapshot: "+err.Error(), false, nil)
	}

	now := time.Now().UTC()
	if request.Clock != nil {
		now = request.Clock.Now().UTC()
	}
	cfg := &config{
		workspace:        workspace,
		headDir:          headDir,
		headModule:       module,
		contract:         document,
		symbol:           symbol,
		claimID:          domain.ClaimID(claim),
		seconds:          seconds,
		proxy:            stepopts.Get(request.Spec.Options, stepopts.Goproxy),
		store:            request.Store,
		scope:            request.Scope,
		tenant:           request.Spec.TenantID,
		runID:            request.Spec.RunID,
		now:              now,
		logger:           request.Logger,
		contractFallback: fallback,
	}
	if request.Staged != nil {
		if staged, ok := request.Staged["contract"]; ok && staged.Ref.Digest != "" {
			ref := staged.Ref
			cfg.contractRef = &ref
		}
	}
	environment, envErr := harness.NewEnvironment(ctx, workspace, cfg.proxy)
	if envErr != nil {
		cfg.toolErr = envErr
	} else {
		cfg.env = environment
	}

	seeds := make([][]byte, 0, len(document.Spec.Seeds))
	for _, seed := range document.Spec.Seeds {
		encoded, encodeErr := encodeSeed(seed.Args)
		if encodeErr != nil {
			continue
		}
		seeds = append(seeds, encoded)
	}
	cfg.seeds = seeds
	cfg.seedCount = int64(len(seeds))
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

func resolveContractPath(request engine.Request, headDir, workspace string) (string, error) {
	roots := []string{headDir, workspace}
	if baseDir, ok := stagedPath(request, "base", stepopts.BaseSnapshot); ok {
		roots = append(roots, baseDir)
	}
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
	for _, root := range roots {
		relative, err := filepath.Rel(root, absolute)
		if err != nil {
			continue
		}
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// loadContract mirrors the differential engine's compatibility loader: the
// platform YAML parser cannot decode seed args into json.RawMessage, so a
// lossless YAML -> JSON decode through contract.Document is used as fallback.
// The fallback still applies contract.Document validation.
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
