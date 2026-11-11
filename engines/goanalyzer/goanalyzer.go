package goanalyzer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

// Engine implements the go-analyzer static-analysis engine.
type Engine struct{}

// New returns a ready-to-register go-analyzer engine.
func New() *Engine { return &Engine{} }

// Name returns the registry name of the engine.
func (e *Engine) Name() string { return EngineName }

// Version returns the engine implementation version.
func (e *Engine) Version() string { return EngineVersion }

// Kinds returns the step kinds handled by the engine.
func (e *Engine) Kinds() []domain.StepKind {
	return []domain.StepKind{domain.StepKindAnalyze, domain.StepKindImpact}
}

// Languages returns the languages handled by the engine.
func (e *Engine) Languages() []string { return []string{language} }

// snapshotSelection is the resolved set of staged snapshots for one step.
type snapshotSelection struct {
	headRoot  string
	baseRoot  string
	headInput engine.StagedInput
	baseInput engine.StagedInput
	hasBase   bool
}

// selectSnapshots validates the staged snapshot directories. When only one
// snapshot is staged it is analyzed on its own and change detection is skipped.
func selectSnapshots(request engine.Request) (snapshotSelection, error) {
	head, hasHead := request.Staged["head"]
	base, hasBase := request.Staged["base"]
	selection := snapshotSelection{headInput: head, baseInput: base, hasBase: hasBase}
	if hasHead && strings.TrimSpace(head.Path) != "" {
		selection.headRoot = head.Path
		if hasBase && strings.TrimSpace(base.Path) != "" {
			selection.baseRoot = base.Path
		}
	} else if hasBase && strings.TrimSpace(base.Path) != "" {
		selection.headRoot = base.Path
		selection.headInput = base
		selection.baseInput = engine.StagedInput{}
		selection.hasBase = false
	} else {
		return snapshotSelection{}, fmt.Errorf("no staged base or head snapshot was provided")
	}
	for name, root := range map[string]string{"head": selection.headRoot, "base": selection.baseRoot} {
		if root == "" {
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			return snapshotSelection{}, fmt.Errorf("staged %s snapshot is unavailable: %w", name, err)
		}
		if !info.IsDir() {
			return snapshotSelection{}, fmt.Errorf("staged %s snapshot is not a directory", name)
		}
	}
	return selection, nil
}

// deadlineContext derives a bounded context from the step deadline, leaving a
// safety margin for artifact upload. An already-expired deadline yields an
// immediately expired context.
func deadlineContext(parent context.Context, deadline time.Time, margin time.Duration) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(parent)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithDeadline(parent, time.Now().Add(-time.Millisecond))
	}
	budget := remaining - margin
	if budget < remaining/2 {
		budget = remaining / 2
	}
	return context.WithTimeout(parent, budget)
}

// commitValue resolves a snapshot commit identifier from options or the staged
// content digest.
func commitValue(option string, input engine.StagedInput) string {
	if option != "" {
		return option
	}
	if input.Ref.Algorithm == "sha256" && input.Ref.Digest != "" {
		return input.Ref.Hex()
	}
	return ""
}

// collectModules derives the module list from loaded packages.
func collectModules(pkgs []*packages.Package, fallbackModule string) []Module {
	seen := make(map[string]Module)
	for _, pkg := range pkgs {
		if pkg.Module == nil || pkg.Module.Path == "" {
			continue
		}
		module := Module{
			Path:      pkg.Module.Path,
			Version:   pkg.Module.Version,
			Main:      pkg.Module.Main,
			GoVersion: pkg.Module.GoVersion,
		}
		if replacement := pkg.Module.Replace; replacement != nil {
			module.Replace = replacement.Path
			if replacement.Version != "" {
				module.Replace += "@" + replacement.Version
			}
		}
		key := module.Path + "@" + module.Version
		seen[key] = module
	}
	if len(seen) == 0 && fallbackModule != "" {
		seen[fallbackModule] = Module{Path: fallbackModule, Main: true}
	}
	out := make([]Module, 0, len(seen))
	for _, module := range seen {
		out = append(out, module)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// contextFailure converts an expired or cancelled context into an engine result.
func contextFailure(workCtx, parent context.Context) (engine.Result, bool) {
	if err := workCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(parent.Err(), context.DeadlineExceeded) {
			return engine.Result{
				Outcome:    domain.OutcomeTimedOut,
				ErrorClass: domain.ErrTimeout,
				Message:    "analysis deadline exceeded",
				Retryable:  false,
			}, true
		}
		return engine.Result{
			Outcome:    domain.OutcomeCancelled,
			ErrorClass: domain.ErrCancelled,
			Message:    "analysis cancelled",
			Retryable:  false,
		}, true
	}
	if err := parent.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return engine.Result{
				Outcome:    domain.OutcomeTimedOut,
				ErrorClass: domain.ErrTimeout,
				Message:    "analysis deadline exceeded",
				Retryable:  false,
			}, true
		}
		return engine.Result{
			Outcome:    domain.OutcomeCancelled,
			ErrorClass: domain.ErrCancelled,
			Message:    "analysis cancelled",
			Retryable:  false,
		}, true
	}
	return engine.Result{}, false
}

// failedResult builds a terminal engine result for an operational failure.
func failedResult(class domain.ErrorClass, message string) engine.Result {
	return engine.Result{
		Outcome:    domain.OutcomeFailed,
		ErrorClass: class,
		Message:    message,
		Retryable:  false,
	}
}

// Execute analyzes the staged snapshots and emits IR, impact and diff artifacts.
func (e *Engine) Execute(ctx context.Context, request engine.Request) (engine.Result, error) {
	started := time.Now()
	if request.Store == nil {
		return failedResult(domain.ErrValidationFailed, "artifact store is required"), nil
	}
	options := request.Spec.Options
	limits := newLimitSet()
	selection, err := selectSnapshots(request)
	if err != nil {
		return failedResult(domain.ErrValidationFailed, err.Error()), nil
	}
	workCtx, cancel := deadlineContext(ctx, request.Spec.Deadline, 2*time.Second)
	defer cancel()

	env, err := prepareAnalyzerEnvironment(workCtx, request.Workspace, options, limits)
	if err != nil {
		if result, ok := contextFailure(workCtx, ctx); ok {
			return result, nil
		}
		return failedResult(domain.ErrSandboxUnavailable, err.Error()), nil
	}

	headModule := moduleDirective(selection.headRoot)
	baseModule := ""
	if selection.baseRoot != "" {
		baseModule = moduleDirective(selection.baseRoot)
	} else {
		limits.add("single snapshot analyzed; change detection and base symbols unavailable")
	}

	// Snapshot metadata is captured before type loading so toolchain-generated
	// go.mod/go.sum updates can never appear as source changes.
	headFiles, headScanTruncated, scanErr := scanSnapshot(workCtx, selection.headRoot, limits)
	if scanErr != nil {
		if result, ok := contextFailure(workCtx, ctx); ok {
			return result, nil
		}
		return failedResult(domain.ErrEngineCrash, fmt.Sprintf("scan head snapshot: %v", scanErr)), nil
	}
	baseFiles := map[string]fileEntry{}
	baseScanTruncated := false
	if selection.baseRoot != "" {
		baseFiles, baseScanTruncated, scanErr = scanSnapshot(workCtx, selection.baseRoot, limits)
		if scanErr != nil {
			if result, ok := contextFailure(workCtx, ctx); ok {
				return result, nil
			}
			return failedResult(domain.ErrEngineCrash, fmt.Sprintf("scan base snapshot: %v", scanErr)), nil
		}
	}
	diffs := []FileDiff{}
	diffTruncated := false
	if selection.baseRoot != "" {
		var diffLimits []string
		diffs, diffLimits, diffTruncated = buildFileDiff(baseFiles, headFiles, baseScanTruncated, headScanTruncated)
		limits.addAll(diffLimits)
	} else if len(headFiles) > 0 {
		limits.add("file diff skipped because no base snapshot was staged")
	}

	var headPackages []*packages.Package
	var headIndex symbolIndex
	callEdges := []CallEdge{}
	dataEdges := []DataEdge{}
	unresolved := []Unresolved{}
	loadedPackages, loadFset, loadErr := loadTypeInfo(workCtx, selection.headRoot, env, limits)
	if loadErr == nil {
		headPackages = loadedPackages
		headIndex = extractTypedSymbols(selection.headRoot, loadedPackages, loadFset, limits)
		callEdges, unresolved = extractTypedCallGraph(selection.headRoot, loadedPackages, loadFset, headIndex, limits)
		dataEdges = extractTypedDataEdges(selection.headRoot, loadedPackages, loadFset, headIndex, limits)
	} else {
		if result, ok := contextFailure(workCtx, ctx); ok {
			return result, nil
		}
		limits.add("go/packages loading failed; using go/parser fallback: " + truncateMessage(loadErr.Error(), 240))
		syntax, syntaxFset, parseErr := parseSnapshotSyntax(workCtx, selection.headRoot, headModule, limits)
		if parseErr != nil {
			if result, ok := contextFailure(workCtx, ctx); ok {
				return result, nil
			}
			return failedResult(domain.ErrEngineCrash, fmt.Sprintf("syntax fallback failed: %v", parseErr)), nil
		}
		headIndex = extractSyntaxSymbols(selection.headRoot, syntax, syntaxFset, limits)
		callEdges, unresolved = extractSyntaxCallGraph(selection.headRoot, syntax, syntaxFset, headIndex, limits)
		limits.add("type resolution unavailable; fallback call edges are APPROX or UNKNOWN only")
	}

	baseIndex := symbolIndex{}
	if selection.baseRoot != "" {
		baseSyntax, baseFset, baseErr := parseSnapshotSyntax(workCtx, selection.baseRoot, baseModule, limits)
		if baseErr != nil {
			if result, ok := contextFailure(workCtx, ctx); ok {
				return result, nil
			}
			limits.add("base symbols could not be reconstructed: " + truncateMessage(baseErr.Error(), 240))
		} else {
			baseIndex = extractSyntaxSymbols(selection.baseRoot, baseSyntax, baseFset, limits)
			limits.add("base revision symbols are reconstructed from syntax for deleted-file analysis")
		}
	}

	changes := []Change{}
	if selection.baseRoot != "" {
		changes, _ = buildChanges(workCtx, selection.baseRoot, selection.headRoot, baseModule, headModule, diffs, baseIndex, headIndex, limits)
		if result, ok := contextFailure(workCtx, ctx); ok {
			return result, nil
		}
	}

	modules := collectModules(headPackages, headModule)
	merged := mergeSymbolIndexes(headIndex, baseIndex)
	if merged.symbols == nil {
		merged = buildSymbolIndex([]Symbol{})
	}
	if len(merged.symbols) > maxSymbols {
		limits.add(fmt.Sprintf("merged symbol table capped at %d entries", maxSymbols))
		merged = buildSymbolIndex(merged.symbols[:maxSymbols])
	}
	ir := newIR()
	ir.BaseCommit = commitValue(stepopts.Get(options, stepopts.BaseCommit), selection.baseInput)
	ir.HeadCommit = commitValue(stepopts.Get(options, stepopts.HeadCommit), selection.headInput)
	ir.Modules = modules
	ir.Symbols = merged.symbols
	ir.CallEdges = nonNilCallEdges(callEdges)
	ir.DataEdges = nonNilDataEdges(dataEdges)
	ir.Changes = nonNilChanges(changes)
	ir.Unresolved = nonNilUnresolved(unresolved)
	scrubbedLimits := func() []string {
		return scrubRoots(limits.list(), selection.headRoot, selection.baseRoot, request.Workspace)
	}
	ir.Limits = scrubbedLimits()
	sortIR(&ir)

	depth := impactDepth(options, limits)
	headSymbols := make(map[string]struct{}, len(headIndex.symbols))
	for _, symbol := range headIndex.symbols {
		headSymbols[symbol.ID] = struct{}{}
	}
	impact := computeImpact(workCtx, selection.headRoot, ir, headSymbols, depth, limits)
	if result, ok := contextFailure(workCtx, ctx); ok {
		return result, nil
	}
	ir.Limits = scrubbedLimits()
	impact.Limits = scrubbedLimits()

	diffBundle := DiffBundle{
		SchemaVersion: DiffSchemaVersion,
		Files:         diffs,
		Truncated:     diffTruncated,
		Limits:        scrubbedLimits(),
	}
	if diffBundle.Files == nil {
		diffBundle.Files = []FileDiff{}
	}

	irRef, err := engine.PutJSON(ctx, request.Store, request.Scope, ir)
	if err != nil {
		return failedResult(domain.ErrCASUnavailable, fmt.Sprintf("write ir.json: %v", err)), nil
	}
	impactRef, err := engine.PutJSON(ctx, request.Store, request.Scope, impact)
	if err != nil {
		return failedResult(domain.ErrCASUnavailable, fmt.Sprintf("write impact.json: %v", err)), nil
	}
	diffRef, err := engine.PutJSON(ctx, request.Store, request.Scope, diffBundle)
	if err != nil {
		return failedResult(domain.ErrCASUnavailable, fmt.Sprintf("write diff.json: %v", err)), nil
	}

	artifacts := []domain.CASRef{irRef, impactRef, diffRef}
	result := engine.Result{
		Outcome:   domain.OutcomeSucceeded,
		Message:   fmt.Sprintf("analyzed %d symbols, %d call edges, %d changed files", len(ir.Symbols), len(ir.CallEdges), len(ir.Changes)),
		Artifacts: artifacts,
		Outputs: map[string]string{
			"ir.digest":        irRef.Hex(),
			"impact.digest":    impactRef.Hex(),
			"diff.digest":      diffRef.Hex(),
			"symbols":          strconv.Itoa(len(ir.Symbols)),
			"call_edges":       strconv.Itoa(len(ir.CallEdges)),
			"data_edges":       strconv.Itoa(len(ir.DataEdges)),
			"unresolved":       strconv.Itoa(len(ir.Unresolved)),
			"changed_files":    strconv.Itoa(len(ir.Changes)),
			"changed_symbols":  strconv.Itoa(len(impact.ChangedSymbols)),
			"affected_symbols": strconv.Itoa(len(impact.AffectedSymbols)),
			"test_candidates":  strconv.Itoa(len(impact.TestCandidates)),
			"resolution_exact": strconv.Itoa(impact.Resolution.Exact),
		},
		Metrics: domain.StepMetrics{ExecutionMS: time.Since(started).Milliseconds()},
	}
	for _, artifact := range artifacts {
		result.Metrics.CASWrittenBytes += artifact.SizeBytes
	}
	if claimID := claimOption(options); claimID != "" {
		evidence, err := buildEvidence(request, claimID, artifacts, scrubbedLimits())
		if err != nil {
			return failedResult(domain.ErrValidationFailed, err.Error()), nil
		}
		result.Evidence = []domain.Evidence{evidence}
	}
	return result, nil
}

// nonNilCallEdges guarantees a JSON array rather than null.
func nonNilCallEdges(values []CallEdge) []CallEdge {
	if values == nil {
		return []CallEdge{}
	}
	return values
}

// nonNilDataEdges guarantees a JSON array rather than null.
func nonNilDataEdges(values []DataEdge) []DataEdge {
	if values == nil {
		return []DataEdge{}
	}
	return values
}

// nonNilChanges guarantees a JSON array rather than null.
func nonNilChanges(values []Change) []Change {
	if values == nil {
		return []Change{}
	}
	return values
}

// nonNilUnresolved guarantees a JSON array rather than null.
func nonNilUnresolved(values []Unresolved) []Unresolved {
	if values == nil {
		return []Unresolved{}
	}
	return values
}
