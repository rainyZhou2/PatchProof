// Package goanalyzer implements the go-analyzer static-analysis engine.
//
// The engine builds a deterministic, language-neutral intermediate
// representation (IR) of one or two Go snapshots, computes file-level changes,
// and derives a bounded reverse-call-graph impact set from those changes.
package goanalyzer

import (
	"path/filepath"
	"sort"
	"strings"
)

const (
	// EngineName is the stable registry name of the engine.
	EngineName = "go-analyzer"
	// EngineVersion is the semantic version of the emitted artifacts.
	EngineVersion = "0.1.0"

	// IRSchemaVersion is the schema identifier of the ir.json artifact.
	IRSchemaVersion = "patchproof.ir/v1"
	// ImpactSchemaVersion is the schema identifier of the impact.json artifact.
	ImpactSchemaVersion = "patchproof.impact/v1"
	// DiffSchemaVersion is the schema identifier of the diff.json artifact.
	DiffSchemaVersion = "patchproof.diff/v1"

	// language is the only language this engine analyzes.
	language = "go"

	defaultImpactDepth = 8
	maxImpactDepth     = 64

	maxSymbols          = 200000
	maxCallEdges        = 1000000
	maxDataEdges        = 200000
	maxUnresolved       = 50000
	maxChanges          = 100000
	maxApproxCandidates = 32
	maxSignatureBytes   = 512
	maxTestCandidates   = 2000
	maxUnknownCallers   = 50000
	maxFileBytes        = 8 << 20
	maxSnapshotFiles    = 200000
)

// Symbol kinds produced by the analyzer.
const (
	SymbolKindFunc   = "func"
	SymbolKindMethod = "method"
	SymbolKindType   = "type"
	SymbolKindVar    = "var"
	SymbolKindConst  = "const"
)

// Resolution values emitted for call edges.
const (
	ResolutionExact   = "EXACT"
	ResolutionApprox  = "APPROX"
	ResolutionUnknown = "UNKNOWN"
)

// Semantic change kinds attached to modified symbols and files.
const (
	SemanticReturnChanged    = "RETURN_EXPR_CHANGED"
	SemanticConditionChanged = "CONDITION_CHANGED"
	SemanticErrorChanged     = "ERROR_HANDLING_CHANGED"
	SemanticSignatureChanged = "API_SIGNATURE_CHANGED"
	SemanticConcurrency      = "CONCURRENCY_CHANGED"
	SemanticDependency       = "DEPENDENCY_CHANGED"
	SemanticSymbolAdded      = "SYMBOL_ADDED"
	SemanticSymbolDeleted    = "SYMBOL_DELETED"
	SemanticBodyChanged      = "SYMBOL_BODY_CHANGED"
	SemanticValueChanged     = "VALUE_CHANGED"
)

// Module is a Go module that contributed packages to the IR.
type Module struct {
	Path      string `json:"path"`
	Version   string `json:"version,omitempty"`
	Main      bool   `json:"main"`
	GoVersion string `json:"go_version,omitempty"`
	Replace   string `json:"replace,omitempty"`
}

// Symbol is a named declaration identified by a stable, package-qualified ID.
type Symbol struct {
	ID          string `json:"id"`
	PackagePath string `json:"package_path"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Signature   string `json:"signature"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	Exported    bool   `json:"exported"`
}

// CallEdge is a directed call relationship. Callee is empty when resolution is
// UNKNOWN; such edges are retained so downstream consumers never silently lose
// dynamic call sites.
type CallEdge struct {
	Caller     string  `json:"caller"`
	Callee     string  `json:"callee"`
	Resolution string  `json:"resolution"`
	Confidence float64 `json:"confidence"`
	File       string  `json:"file"`
	Line       int     `json:"line"`
}

// DataEdge records a coarse value-flow relationship between symbols.
type DataEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// Change is a file-level change enriched with the symbols it touches and the
// semantic categories detected between the base and head revisions.
type Change struct {
	Path          string   `json:"path"`
	Kind          string   `json:"kind"`
	BaseDigest    string   `json:"base_digest,omitempty"`
	HeadDigest    string   `json:"head_digest,omitempty"`
	Symbols       []string `json:"symbols"`
	SemanticKinds []string `json:"semantic_kinds"`
}

// Unresolved records a call site the analyzer could not bind statically.
type Unresolved struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	Reason   string `json:"reason"`
}

// IRBundle is the language-neutral analysis artifact.
type IRBundle struct {
	SchemaVersion string       `json:"schema_version"`
	Language      string       `json:"language"`
	BaseCommit    string       `json:"base_commit"`
	HeadCommit    string       `json:"head_commit"`
	Modules       []Module     `json:"modules"`
	Symbols       []Symbol     `json:"symbols"`
	CallEdges     []CallEdge   `json:"call_edges"`
	DataEdges     []DataEdge   `json:"data_edges"`
	Changes       []Change     `json:"changes"`
	Unresolved    []Unresolved `json:"unresolved"`
	Limits        []string     `json:"limits"`
}

// Coverage is the resolution-coverage histogram of the analyzed call graph.
type Coverage struct {
	Exact   int `json:"exact"`
	Approx  int `json:"approx"`
	Unknown int `json:"unknown"`
}

// ImpactBundle is the reverse-reachability impact artifact.
type ImpactBundle struct {
	SchemaVersion    string   `json:"schema_version"`
	ChangedFiles     []string `json:"changed_files"`
	ChangedSymbols   []string `json:"changed_symbols"`
	AffectedSymbols  []string `json:"affected_symbols"`
	AffectedPackages []string `json:"affected_packages"`
	TestCandidates   []string `json:"test_candidates"`
	Resolution       Coverage `json:"resolution"`
	Limits           []string `json:"limits"`
	Depth            int      `json:"depth"`
	Truncated        bool     `json:"truncated"`
}

// FileDiff describes one changed file with digest, size, mode and binary
// classification. The digest is computed from the full file contents, while
// content reads used for parsing are bounded by maxFileBytes.
type FileDiff struct {
	Path       string `json:"path"`
	Change     string `json:"change"`
	OldPath    string `json:"old_path,omitempty"`
	BaseDigest string `json:"base_digest,omitempty"`
	HeadDigest string `json:"head_digest,omitempty"`
	BaseSize   int64  `json:"base_size,omitempty"`
	HeadSize   int64  `json:"head_size,omitempty"`
	BaseMode   string `json:"base_mode,omitempty"`
	HeadMode   string `json:"head_mode,omitempty"`
	Binary     bool   `json:"binary"`
}

// DiffBundle is the file-level diff artifact.
type DiffBundle struct {
	SchemaVersion string     `json:"schema_version"`
	Files         []FileDiff `json:"files"`
	Truncated     bool       `json:"truncated"`
	Limits        []string   `json:"limits"`
}

// newIR returns an IR bundle with non-nil slices so the JSON encoding is stable.
func newIR() IRBundle {
	return IRBundle{
		SchemaVersion: IRSchemaVersion,
		Language:      language,
		Modules:       []Module{},
		Symbols:       []Symbol{},
		CallEdges:     []CallEdge{},
		DataEdges:     []DataEdge{},
		Changes:       []Change{},
		Unresolved:    []Unresolved{},
		Limits:        []string{},
	}
}

// newImpact returns an impact bundle with non-nil slices.
func newImpact(depth int) ImpactBundle {
	return ImpactBundle{
		SchemaVersion:    ImpactSchemaVersion,
		ChangedFiles:     []string{},
		ChangedSymbols:   []string{},
		AffectedSymbols:  []string{},
		AffectedPackages: []string{},
		TestCandidates:   []string{},
		Limits:           []string{},
		Depth:            depth,
	}
}

// limitSet accumulates deterministic, de-duplicated limitation messages.
type limitSet struct {
	values []string
	seen   map[string]struct{}
}

func newLimitSet() *limitSet {
	return &limitSet{seen: map[string]struct{}{}}
}

func (l *limitSet) add(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if _, ok := l.seen[message]; ok {
		return
	}
	l.seen[message] = struct{}{}
	l.values = append(l.values, message)
}

func (l *limitSet) addAll(messages []string) {
	for _, message := range messages {
		l.add(message)
	}
}

func (l *limitSet) list() []string {
	out := append([]string(nil), l.values...)
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}

// sortIR sorts every slice of the bundle so artifacts are byte-for-byte stable.
func sortIR(bundle *IRBundle) {
	sort.Slice(bundle.Modules, func(i, j int) bool {
		left, right := bundle.Modules[i], bundle.Modules[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Version != right.Version {
			return left.Version < right.Version
		}
		return left.Main && !right.Main
	})
	sort.Slice(bundle.Symbols, func(i, j int) bool {
		left, right := bundle.Symbols[i], bundle.Symbols[j]
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		if left.File != right.File {
			return left.File < right.File
		}
		return left.Line < right.Line
	})
	sort.Slice(bundle.CallEdges, func(i, j int) bool {
		left, right := bundle.CallEdges[i], bundle.CallEdges[j]
		if left.Caller != right.Caller {
			return left.Caller < right.Caller
		}
		if left.Callee != right.Callee {
			return left.Callee < right.Callee
		}
		if left.File != right.File {
			return left.File < right.File
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Resolution < right.Resolution
	})
	sort.Slice(bundle.DataEdges, func(i, j int) bool {
		left, right := bundle.DataEdges[i], bundle.DataEdges[j]
		if left.From != right.From {
			return left.From < right.From
		}
		if left.To != right.To {
			return left.To < right.To
		}
		if left.File != right.File {
			return left.File < right.File
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Kind < right.Kind
	})
	sort.Slice(bundle.Changes, func(i, j int) bool { return bundle.Changes[i].Path < bundle.Changes[j].Path })
	sort.Slice(bundle.Unresolved, func(i, j int) bool {
		left, right := bundle.Unresolved[i], bundle.Unresolved[j]
		if left.FilePath != right.FilePath {
			return left.FilePath < right.FilePath
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Reason < right.Reason
	})
	sort.Strings(bundle.Limits)
}

// sortImpact sorts every slice of the impact bundle.
func sortImpact(bundle *ImpactBundle) {
	sort.Strings(bundle.ChangedFiles)
	sort.Strings(bundle.ChangedSymbols)
	sort.Strings(bundle.AffectedSymbols)
	sort.Strings(bundle.AffectedPackages)
	sort.Strings(bundle.TestCandidates)
	sort.Strings(bundle.Limits)
}

// dedupeStrings sorts and de-duplicates a string slice in place semantics and
// always returns a non-nil slice.
func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	write := 1
	for read := 1; read < len(out); read++ {
		if out[read] != out[write-1] {
			out[write] = out[read]
			write++
		}
	}
	return out[:write]
}

// scrubRoots removes absolute snapshot and workspace prefixes from
// user-facing text so artifacts remain stable across checkout locations.
func scrubRoots(values []string, roots ...string) []string {
	prepared := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" || root == string(filepath.Separator) {
			continue
		}
		prepared = append(prepared, filepath.Clean(root))
	}
	sort.Slice(prepared, func(i, j int) bool { return len(prepared[i]) > len(prepared[j]) })
	out := make([]string, 0, len(values))
	for _, value := range values {
		scrubbed := value
		for _, root := range prepared {
			scrubbed = strings.ReplaceAll(scrubbed, root, ".")
			if slash := filepath.ToSlash(root); slash != root {
				scrubbed = strings.ReplaceAll(scrubbed, slash, ".")
			}
		}
		out = append(out, scrubbed)
	}
	return out
}
