package goanalyzer

import (
	"container/heap"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patchproof/patchproof/pkg/stepopts"
)

// impactNode is a frontier entry for the confidence-weighted reverse BFS.
type impactNode struct {
	id     string
	depth  int
	weight float64
}

// impactQueue is a max-heap ordered by weight, then depth, then identifier.
type impactQueue []impactNode

func (q impactQueue) Len() int { return len(q) }
func (q impactQueue) Less(i, j int) bool {
	if q[i].weight != q[j].weight {
		return q[i].weight > q[j].weight
	}
	if q[i].depth != q[j].depth {
		return q[i].depth < q[j].depth
	}
	return q[i].id < q[j].id
}
func (q impactQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *impactQueue) Push(value any) {
	*q = append(*q, value.(impactNode))
}
func (q *impactQueue) Pop() any {
	old := *q
	node := old[len(old)-1]
	*q = old[:len(old)-1]
	return node
}

// impactDepth resolves the effective reverse-BFS depth from step options.
func impactDepth(options map[string]string, limits *limitSet) int {
	raw := stepopts.GetInt64(options, "impact.depth", defaultImpactDepth)
	depth := int(raw)
	if depth <= 0 {
		limits.add(fmt.Sprintf("invalid impact.depth %d; using default %d", raw, defaultImpactDepth))
		return defaultImpactDepth
	}
	if depth > maxImpactDepth {
		limits.add(fmt.Sprintf("impact.depth %d capped at %d", depth, maxImpactDepth))
		return maxImpactDepth
	}
	return depth
}

// packageOfSymbolID extracts the import path prefix of a symbol ID.
func packageOfSymbolID(id string) string {
	if index := strings.Index(id, "#"); index >= 0 {
		return id[:index]
	}
	return id
}

// computeImpact derives the reverse-reachability impact set for the IR.
func computeImpact(ctx context.Context, root string, bundle IRBundle, headSymbols map[string]struct{}, depth int, limits *limitSet) ImpactBundle {
	impact := newImpact(depth)
	impact.Limits = []string{}
	byID := make(map[string]Symbol, len(bundle.Symbols))
	for _, symbol := range bundle.Symbols {
		byID[symbol.ID] = symbol
	}
	changedFiles := make(map[string]struct{})
	changedSymbolSet := make(map[string]struct{})
	for _, change := range bundle.Changes {
		changedFiles[change.Path] = struct{}{}
		for _, symbol := range change.Symbols {
			changedSymbolSet[symbol] = struct{}{}
		}
	}
	impact.ChangedFiles = sortedKeys(changedFiles)
	impact.ChangedSymbols = sortedKeys(changedSymbolSet)
	if len(impact.ChangedSymbols) == 0 {
		impact.Limits = limits.list()
		return impact
	}

	reverse := make(map[string][]CallEdge, len(bundle.CallEdges))
	unknownCallers := make(map[string]struct{})
	for _, edge := range bundle.CallEdges {
		switch edge.Resolution {
		case ResolutionExact:
			impact.Resolution.Exact++
		case ResolutionApprox:
			impact.Resolution.Approx++
		default:
			impact.Resolution.Unknown++
		}
		if edge.Callee == "" {
			if edge.Caller != "" {
				unknownCallers[edge.Caller] = struct{}{}
			}
			continue
		}
		reverse[edge.Callee] = append(reverse[edge.Callee], edge)
	}

	affected := make(map[string]int, len(impact.ChangedSymbols))
	truncated := false
	add := func(id string, nodeDepth int) bool {
		if id == "" {
			return false
		}
		if _, ok := affected[id]; ok {
			return false
		}
		if len(affected) >= maxSymbols {
			truncated = true
			return false
		}
		affected[id] = nodeDepth
		return true
	}

	queue := &impactQueue{}
	heap.Init(queue)
	bestDepth := make(map[string]int, len(impact.ChangedSymbols))
	bestWeight := make(map[string]float64, len(impact.ChangedSymbols))
	for _, seed := range impact.ChangedSymbols {
		if !add(seed, 0) {
			continue
		}
		bestDepth[seed] = 0
		bestWeight[seed] = 1.0
		heap.Push(queue, impactNode{id: seed, depth: 0, weight: 1.0})
	}

	// Seeds without a head definition (deleted files or test-only symbols) have
	// no head call edges. Conservatively treat every head symbol in the same
	// package as potentially affected.
	deletedSeed := false
	for _, seed := range impact.ChangedSymbols {
		if _, ok := headSymbols[seed]; ok {
			continue
		}
		deletedSeed = true
		pkgPath := packageOfSymbolID(seed)
		for id, symbol := range byID {
			if symbol.PackagePath != pkgPath {
				continue
			}
			if !add(id, 1) {
				continue
			}
			bestDepth[id] = 1
			bestWeight[id] = 0.9
			heap.Push(queue, impactNode{id: id, depth: 1, weight: 0.9})
		}
	}
	if deletedSeed {
		limits.add("symbols without a head definition were expanded to their whole package conservatively")
	}

	for queue.Len() > 0 {
		if err := ctx.Err(); err != nil {
			limits.add("impact analysis stopped early because the step deadline expired")
			break
		}
		node := heap.Pop(queue).(impactNode)
		if node.depth >= depth {
			continue
		}
		for _, edge := range reverse[node.id] {
			if err := ctx.Err(); err != nil {
				limits.add("impact analysis stopped early because the step deadline expired")
				break
			}
			nextDepth := node.depth + 1
			nextWeight := node.weight * edge.Confidence
			previousDepth, seen := bestDepth[edge.Caller]
			if seen {
				if previousDepth < nextDepth {
					continue
				}
				if previousDepth == nextDepth && bestWeight[edge.Caller] >= nextWeight {
					continue
				}
			}
			if !add(edge.Caller, nextDepth) {
				continue
			}
			bestDepth[edge.Caller] = nextDepth
			bestWeight[edge.Caller] = nextWeight
			heap.Push(queue, impactNode{id: edge.Caller, depth: nextDepth, weight: nextWeight})
		}
	}
	if truncated {
		limits.add(fmt.Sprintf("impact set capped at %d symbols", maxSymbols))
	}

	// Any call site that could not be resolved may dynamically target a changed
	// symbol, so its caller is included conservatively.
	if len(unknownCallers) > 0 {
		added := 0
		for caller := range unknownCallers {
			if added >= maxUnknownCallers {
				truncated = true
				limits.add(fmt.Sprintf("unknown-call caller expansion capped at %d symbols", maxUnknownCallers))
				break
			}
			if add(caller, 1) {
				added++
			}
		}
		if added > 0 {
			limits.add("callers of unresolved dynamic calls were added conservatively to the impact set")
		}
	}

	affectedSet := make(map[string]struct{}, len(affected))
	for id := range affected {
		affectedSet[id] = struct{}{}
	}
	impact.AffectedSymbols = sortedKeys(affectedSet)
	packageSet := make(map[string]struct{}, len(impact.AffectedSymbols))
	for _, id := range impact.AffectedSymbols {
		if symbol, ok := byID[id]; ok {
			packageSet[symbol.PackagePath] = struct{}{}
			continue
		}
		if pkgPath := packageOfSymbolID(id); pkgPath != "" {
			packageSet[pkgPath] = struct{}{}
		}
	}
	impact.AffectedPackages = sortedKeys(packageSet)
	impact.TestCandidates = collectTestCandidates(ctx, root, impact.AffectedSymbols, byID, limits)
	impact.Truncated = truncated
	impact.Limits = limits.list()
	return impact
}

// affectedNames derives the identifier names referenced by affected symbols.
func affectedNames(ids []string, byID map[string]Symbol) map[string]struct{} {
	names := make(map[string]struct{}, len(ids)*2)
	for _, id := range ids {
		if symbol, ok := byID[id]; ok && symbol.Name != "" {
			names[symbol.Name] = struct{}{}
		}
		suffix := id
		if index := strings.Index(suffix, "#"); index >= 0 {
			suffix = suffix[index+1:]
		}
		if suffix != "" {
			names[suffix] = struct{}{}
			if index := strings.LastIndex(suffix, "."); index >= 0 && index+1 < len(suffix) {
				names[suffix[index+1:]] = struct{}{}
			}
		}
	}
	return names
}

// collectTestCandidates scans Go test files for static references to affected
// symbol names and reports test functions or files that must be re-run.
func collectTestCandidates(ctx context.Context, root string, affected []string, byID map[string]Symbol, limits *limitSet) []string {
	names := affectedNames(affected, byID)
	if len(names) == 0 {
		return []string{}
	}
	candidates := make(map[string]struct{})
	parseFailures := 0
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			limits.add(fmt.Sprintf("test candidate scan skipped %s: %v", relativePath(root, path), err))
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			if fallbackSkippedDirNames[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		relative := relativePath(root, path)
		if !isTestSource(relative) || isVendoredPath(relative) {
			return nil
		}
		content, bounded, err := readFileBounded(path)
		if err != nil {
			limits.add(fmt.Sprintf("test candidate scan could not read %s: %v", relative, err))
			return nil
		}
		if bounded {
			limits.add(fmt.Sprintf("test candidate scan truncated %s at %d bytes", relative, maxFileBytes))
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, relative, content, parser.SkipObjectResolution)
		if file == nil {
			parseFailures++
			if parseFailures <= 5 {
				limits.add(fmt.Sprintf("test candidate scan could not parse %s: %v", relative, parseErr))
			}
			return nil
		}
		fileHasReference := false
		for _, declaration := range file.Decls {
			funcDecl, ok := declaration.(*ast.FuncDecl)
			if !ok || funcDecl.Body == nil {
				continue
			}
			if !referencesAny(funcDecl.Body, names) {
				continue
			}
			if isTestFunction(funcDecl.Name.Name) {
				candidates[relative+"::"+funcDecl.Name.Name] = struct{}{}
				continue
			}
			// Helpers, init functions and other declarations in a test file are
			// reported at file granularity.
			fileHasReference = true
		}
		if fileHasReference {
			candidates[relative] = struct{}{}
		}
		return nil
	})
	if walkErr != nil && ctx.Err() == nil {
		limits.add(fmt.Sprintf("test candidate scan incomplete: %v", walkErr))
	}
	if len(candidates) > maxTestCandidates {
		limits.add(fmt.Sprintf("test candidate list capped at %d entries", maxTestCandidates))
	}
	out := make([]string, 0, len(candidates))
	for candidate := range candidates {
		out = append(out, candidate)
	}
	sort.Strings(out)
	if len(out) > maxTestCandidates {
		out = out[:maxTestCandidates]
	}
	if len(out) > 0 {
		limits.add("test candidates are selected by name-based static reference scanning")
	}
	return out
}

// referencesAny reports whether a node mentions one of the supplied names.
func referencesAny(node ast.Node, names map[string]struct{}) bool {
	found := false
	ast.Inspect(node, func(current ast.Node) bool {
		if found {
			return false
		}
		switch typed := current.(type) {
		case *ast.Ident:
			if _, ok := names[typed.Name]; ok {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if _, ok := names[typed.Sel.Name]; ok {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// isTestFunction reports whether a function name follows go test conventions.
func isTestFunction(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if name == prefix {
			return true
		}
		if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
			continue
		}
		next := rune(name[len(prefix)])
		if next < 'a' || next > 'z' {
			if next != '_' {
				return true
			}
		}
	}
	return false
}
