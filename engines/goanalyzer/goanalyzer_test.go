package goanalyzer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

const fixtureModule = "module example.com/fixture\n\ngo 1.21\n"

// fixtureSnapshots creates a base/head pair with a value change, a method change,
// an added function and an interface call site.
func fixtureSnapshots(t *testing.T) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	baseDir := filepath.Join(workspace, "base")
	headDir := filepath.Join(workspace, "head")
	baseSource := `package pkg

func Add(a, b int) int {
	return a + b
}

func Caller() int {
	x := Add(1, 2)
	return x
}

type T struct{}

func (t *T) M() int {
	return 1
}
`
	headSource := `package pkg

func Add(a, b int) int {
	return a + b + 1
}

func Caller() int {
	x := Add(1, 2)
	return x
}

type T struct{}

func (t *T) M() int {
	return 2
}

func NewFunc() string {
	return "new"
}

type Runner interface {
	Run() int
}

type Impl struct{}

func (Impl) Run() int {
	return 1
}

func UseRunner(r Runner) int {
	return r.Run()
}
`
	testSource := `package pkg

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 4 {
		t.Fatal("unexpected sum")
	}
}
`
	writeTree(t, baseDir, map[string]string{
		"go.mod":         fixtureModule,
		"pkg/fixture.go": baseSource,
	})
	writeTree(t, headDir, map[string]string{
		"go.mod":              fixtureModule,
		"pkg/fixture.go":      headSource,
		"pkg/fixture_test.go": testSource,
	})
	return baseDir, headDir
}

func TestAnalyzeBaseHead(t *testing.T) {
	baseDir, headDir := fixtureSnapshots(t)
	staged := map[string]engine.StagedInput{}
	staged["base"] = stagedDir(baseDir, strings.Repeat("a", 64))
	staged["head"] = stagedDir(headDir, strings.Repeat("b", 64))
	request, store := testRequest(t, staged, nil)

	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s, message = %s", result.Outcome, result.Message)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("evidence must not be emitted without claim.id, got %d", len(result.Evidence))
	}
	if len(result.Artifacts) != 3 {
		t.Fatalf("artifacts = %d, want 3", len(result.Artifacts))
	}

	ir := decodeArtifact[IRBundle](t, store, result, "ir.digest")
	if ir.SchemaVersion != IRSchemaVersion || ir.Language != "go" {
		t.Fatalf("unexpected IR schema/language: %+v", ir)
	}
	if len(ir.Modules) != 1 || ir.Modules[0].Path != "example.com/fixture" || !ir.Modules[0].Main {
		t.Fatalf("modules = %+v", ir.Modules)
	}
	for _, id := range []string{
		"example.com/fixture/pkg#Add",
		"example.com/fixture/pkg#Caller",
		"example.com/fixture/pkg#T.M",
		"example.com/fixture/pkg#T",
		"example.com/fixture/pkg#Runner",
		"example.com/fixture/pkg#NewFunc",
	} {
		if !hasSymbol(ir, id) {
			t.Errorf("IR is missing symbol %s", id)
		}
	}
	if !hasEdge(ir, "example.com/fixture/pkg#Caller", "example.com/fixture/pkg#Add", ResolutionExact) {
		t.Errorf("missing exact call edge Caller -> Add")
	}
	if !hasEdge(ir, "example.com/fixture/pkg#UseRunner", "example.com/fixture/pkg#Impl.Run", ResolutionApprox) {
		t.Errorf("missing approximate interface edge UseRunner -> Impl.Run")
	}
	foundDataEdge := false
	for _, edge := range ir.DataEdges {
		if edge.From == "example.com/fixture/pkg#Add" && edge.To == "example.com/fixture/pkg#Caller" && edge.Kind == "RESULT_FLOW" {
			foundDataEdge = true
		}
	}
	if !foundDataEdge {
		t.Errorf("missing data edge Add -> Caller, edges=%+v", ir.DataEdges)
	}
	if !hasString(ir.Limits, "base revision symbols are reconstructed from syntax for deleted-file analysis") {
		t.Errorf("missing base-symbol limitation, limits=%v", ir.Limits)
	}

	var fixtureChange *Change
	for index := range ir.Changes {
		if ir.Changes[index].Path == "pkg/fixture.go" {
			fixtureChange = &ir.Changes[index]
		}
	}
	if fixtureChange == nil {
		t.Fatalf("missing change for pkg/fixture.go, changes=%+v", ir.Changes)
	}
	if fixtureChange.Kind != "MODIFIED" {
		t.Errorf("change kind = %s", fixtureChange.Kind)
	}
	for _, id := range []string{"example.com/fixture/pkg#Add", "example.com/fixture/pkg#T.M", "example.com/fixture/pkg#NewFunc"} {
		if !hasString(fixtureChange.Symbols, id) {
			t.Errorf("change symbols missing %s: %v", id, fixtureChange.Symbols)
		}
	}
	if !hasString(fixtureChange.SemanticKinds, SemanticReturnChanged) {
		t.Errorf("semantic kinds missing RETURN_EXPR_CHANGED: %v", fixtureChange.SemanticKinds)
	}
	if !hasString(fixtureChange.SemanticKinds, SemanticSymbolAdded) {
		t.Errorf("semantic kinds missing SYMBOL_ADDED: %v", fixtureChange.SemanticKinds)
	}

	impact := decodeArtifact[ImpactBundle](t, store, result, "impact.digest")
	if impact.SchemaVersion != ImpactSchemaVersion || impact.Depth != defaultImpactDepth {
		t.Fatalf("unexpected impact metadata: %+v", impact)
	}
	if !hasString(impact.ChangedSymbols, "example.com/fixture/pkg#Add") {
		t.Errorf("changed symbols missing Add: %v", impact.ChangedSymbols)
	}
	if !hasString(impact.AffectedSymbols, "example.com/fixture/pkg#Add") ||
		!hasString(impact.AffectedSymbols, "example.com/fixture/pkg#Caller") {
		t.Errorf("affected symbols missing Add/Caller: %v", impact.AffectedSymbols)
	}
	if !hasString(impact.TestCandidates, "pkg/fixture_test.go::TestAdd") {
		t.Errorf("missing test candidate: %v", impact.TestCandidates)
	}
	if impact.Resolution.Exact == 0 {
		t.Errorf("resolution coverage should include exact edges: %+v", impact.Resolution)
	}
	if impact.Truncated {
		t.Errorf("impact should not be truncated")
	}

	diff := decodeArtifact[DiffBundle](t, store, result, "diff.digest")
	if len(diff.Files) != 2 {
		t.Fatalf("diff files = %+v", diff.Files)
	}
	for _, file := range diff.Files {
		if file.Path == "pkg/fixture.go" && (file.Change != "MODIFIED" || file.BaseDigest == "" || file.HeadDigest == "") {
			t.Errorf("unexpected fixture.go diff: %+v", file)
		}
	}
}

func TestAnalyzeDeterministic(t *testing.T) {
	baseDir, headDir := fixtureSnapshots(t)
	staged := map[string]engine.StagedInput{}
	staged["base"] = stagedDir(baseDir, strings.Repeat("c", 64))
	staged["head"] = stagedDir(headDir, strings.Repeat("d", 64))

	firstRequest, _ := testRequest(t, staged, nil)
	first, err := New().Execute(context.Background(), firstRequest)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	secondRequest, _ := testRequest(t, staged, nil)
	second, err := New().Execute(context.Background(), secondRequest)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	for _, key := range []string{"ir.digest", "impact.digest", "diff.digest"} {
		if first.Outputs[key] != second.Outputs[key] {
			t.Errorf("%s is not deterministic: %s != %s", key, first.Outputs[key], second.Outputs[key])
		}
	}
}

func TestAnalyzeEvidenceForClaim(t *testing.T) {
	baseDir, headDir := fixtureSnapshots(t)
	staged := map[string]engine.StagedInput{}
	staged["base"] = stagedDir(baseDir, strings.Repeat("e", 64))
	staged["head"] = stagedDir(headDir, strings.Repeat("f", 64))
	request, _ := testRequest(t, staged, map[string]string{"claim.id": "fixture.claim"})

	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence count = %d, want 1", len(result.Evidence))
	}
	evidence := result.Evidence[0]
	if evidence.Kind != domain.EvidenceStatic || evidence.Result != domain.ClaimTested {
		t.Errorf("unexpected evidence kind/result: %s/%s", evidence.Kind, evidence.Result)
	}
	if evidence.ClaimID != "fixture.claim" {
		t.Errorf("claim id = %s", evidence.ClaimID)
	}
	if evidence.Scope.InputDomain != "changed symbols + bounded reverse call graph" {
		t.Errorf("scope = %q", evidence.Scope.InputDomain)
	}
	if evidence.Engine.Name != EngineName || evidence.Engine.Version != EngineVersion {
		t.Errorf("engine = %+v", evidence.Engine)
	}
	if evidence.Environment.Arch != runtime.GOARCH {
		t.Errorf("arch = %s", evidence.Environment.Arch)
	}
	if evidence.Environment.SandboxProfile != string(domain.IsolationContainer) {
		t.Errorf("sandbox profile = %s", evidence.Environment.SandboxProfile)
	}
	if len(evidence.Artifacts) != 3 {
		t.Errorf("evidence artifacts = %d, want 3", len(evidence.Artifacts))
	}
	if len(evidence.Assumptions) == 0 || !strings.Contains(evidence.Assumptions[0], "go/packages") {
		t.Errorf("assumptions = %v", evidence.Assumptions)
	}
	if err := evidence.Validate(); err != nil {
		t.Errorf("evidence does not validate: %v", err)
	}
}

func TestAnalyzeHeadOnly(t *testing.T) {
	_, headDir := fixtureSnapshots(t)
	request, store := testRequest(t, map[string]engine.StagedInput{
		"head": stagedDir(headDir, strings.Repeat("1", 64)),
	}, nil)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s message = %s", result.Outcome, result.Message)
	}
	ir := decodeArtifact[IRBundle](t, store, result, "ir.digest")
	if len(ir.Changes) != 0 {
		t.Errorf("head-only analysis must not report changes: %+v", ir.Changes)
	}
	if !hasSymbol(ir, "example.com/fixture/pkg#Add") {
		t.Errorf("head-only analysis lost symbols")
	}
	diff := decodeArtifact[DiffBundle](t, store, result, "diff.digest")
	if len(diff.Files) != 0 {
		t.Errorf("head-only diff should be empty: %+v", diff.Files)
	}
}

func TestAnalyzeDeletedFile(t *testing.T) {
	workspace := t.TempDir()
	baseDir := filepath.Join(workspace, "base")
	headDir := filepath.Join(workspace, "head")
	writeTree(t, baseDir, map[string]string{
		"go.mod":         fixtureModule,
		"pkg/fixture.go": "package pkg\n\nfunc Add(a, b int) int { return a + b }\n",
		"pkg/extra.go":   "package pkg\n\nfunc Removed() int { return 1 }\n",
	})
	writeTree(t, headDir, map[string]string{
		"go.mod":         fixtureModule,
		"pkg/fixture.go": "package pkg\n\nfunc Add(a, b int) int { return a + b }\n",
	})
	staged := map[string]engine.StagedInput{}
	staged["base"] = stagedDir(baseDir, strings.Repeat("2", 64))
	staged["head"] = stagedDir(headDir, strings.Repeat("3", 64))
	request, store := testRequest(t, staged, nil)

	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ir := decodeArtifact[IRBundle](t, store, result, "ir.digest")
	var deleted *Change
	for index := range ir.Changes {
		if ir.Changes[index].Path == "pkg/extra.go" {
			deleted = &ir.Changes[index]
		}
	}
	if deleted == nil || deleted.Kind != "DELETED" {
		t.Fatalf("missing deletion change: %+v", ir.Changes)
	}
	if !hasString(deleted.Symbols, "example.com/fixture/pkg#Removed") {
		t.Errorf("deleted change symbols = %v", deleted.Symbols)
	}
	if !hasString(deleted.SemanticKinds, SemanticSymbolDeleted) {
		t.Errorf("deleted semantic kinds = %v", deleted.SemanticKinds)
	}
	impact := decodeArtifact[ImpactBundle](t, store, result, "impact.digest")
	if !hasString(impact.AffectedSymbols, "example.com/fixture/pkg#Add") {
		t.Errorf("conservative package impact missing head symbols: %v", impact.AffectedSymbols)
	}
}

func TestAnalyzeBrokenImportKeepsIR(t *testing.T) {
	workspace := t.TempDir()
	headDir := filepath.Join(workspace, "head")
	writeTree(t, headDir, map[string]string{
		"go.mod": fixtureModule,
		"pkg/fixture.go": `package pkg

import "example.invalid/missing"

func Add(a, b int) int { return a + b }

func Caller() int { return Add(1, 2) + missing.Value }
`,
	})
	request, store := testRequest(t, map[string]engine.StagedInput{
		"head": stagedDir(headDir, strings.Repeat("4", 64)),
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := New().Execute(ctx, request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s message = %s", result.Outcome, result.Message)
	}
	ir := decodeArtifact[IRBundle](t, store, result, "ir.digest")
	if len(ir.Limits) == 0 {
		t.Errorf("expected limitations for broken import")
	}
	if len(ir.Symbols) == 0 {
		t.Errorf("expected symbols despite broken import")
	}
}

func TestAnalyzeSyntaxFallbackWithoutModule(t *testing.T) {
	workspace := t.TempDir()
	headDir := filepath.Join(workspace, "head")
	writeTree(t, headDir, map[string]string{
		"pkg/fixture.go": `package pkg

func Add(a, b int) int { return a + b }

func Caller() int { return Add(1, 2) }

func (t *T) M() int { return 1 }

type T struct{}
`,
	})
	request, store := testRequest(t, map[string]engine.StagedInput{
		"head": stagedDir(headDir, strings.Repeat("5", 64)),
	}, nil)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s message = %s", result.Outcome, result.Message)
	}
	ir := decodeArtifact[IRBundle](t, store, result, "ir.digest")
	fallback := false
	for _, limit := range ir.Limits {
		if strings.Contains(limit, "go/packages loading failed") {
			fallback = true
		}
	}
	if !fallback {
		t.Errorf("expected go/packages fallback limitation: %v", ir.Limits)
	}
	if !hasSymbol(ir, "local/pkg#Add") || !hasSymbol(ir, "local/pkg#T.M") {
		t.Errorf("fallback symbols missing: %+v", ir.Symbols)
	}
	for _, edge := range ir.CallEdges {
		if edge.Resolution == ResolutionExact {
			t.Errorf("syntax fallback must not emit exact edges: %+v", edge)
		}
	}
}

func TestAnalyzeInvalidSnapshots(t *testing.T) {
	request, _ := testRequest(t, map[string]engine.StagedInput{}, nil)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeFailed || result.ErrorClass != domain.ErrValidationFailed {
		t.Fatalf("unexpected result: %+v", result)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	request, _ = testRequest(t, map[string]engine.StagedInput{
		"head": stagedDir(missing, strings.Repeat("6", 64)),
	}, nil)
	result, err = New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeFailed || result.ErrorClass != domain.ErrValidationFailed {
		t.Fatalf("unexpected missing-snapshot result: %+v", result)
	}
}

func TestHelperIsGoSource(t *testing.T) {
	cases := map[string]bool{
		"a.go":        true,
		"a_test.go":   false,
		"a.txt":       false,
		"pkg/b.go":    true,
		"vendor/c.go": true,
	}
	for path, want := range cases {
		if got := isGoSource(path); got != want {
			t.Errorf("isGoSource(%q) = %v, want %v", path, got, want)
		}
	}
	if !isVendoredPath("vendor/x/y.go") {
		t.Errorf("vendor path not detected")
	}
	if isVendoredPath("pkg/y.go") {
		t.Errorf("regular path treated as vendored")
	}
	if got := moduleDirective(t.TempDir()); got != "" {
		t.Errorf("moduleDirective(empty) = %q", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(fixtureModule), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := moduleDirective(dir); got != "example.com/fixture" {
		t.Errorf("moduleDirective = %q", got)
	}
}

func TestAnalyzeDeterministicAcrossCheckouts(t *testing.T) {
	firstBase, firstHead := fixtureSnapshots(t)
	secondBase, secondHead := fixtureSnapshots(t)
	buildStaged := func(baseDir, headDir string) map[string]engine.StagedInput {
		return map[string]engine.StagedInput{
			"base": stagedDir(baseDir, strings.Repeat("7", 64)),
			"head": stagedDir(headDir, strings.Repeat("8", 64)),
		}
	}
	firstRequest, _ := testRequest(t, buildStaged(firstBase, firstHead), nil)
	first, err := New().Execute(context.Background(), firstRequest)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	secondRequest, _ := testRequest(t, buildStaged(secondBase, secondHead), nil)
	second, err := New().Execute(context.Background(), secondRequest)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	for _, key := range []string{"ir.digest", "impact.digest", "diff.digest"} {
		if first.Outputs[key] != second.Outputs[key] {
			t.Errorf("%s depends on the checkout location: %s != %s", key, first.Outputs[key], second.Outputs[key])
		}
	}
}

func TestAnalyzeImpactDepthOption(t *testing.T) {
	baseDir, headDir := fixtureSnapshots(t)
	staged := map[string]engine.StagedInput{
		"base": stagedDir(baseDir, strings.Repeat("9", 64)),
		"head": stagedDir(headDir, strings.Repeat("a", 64)),
	}
	request, store := testRequest(t, staged, map[string]string{"impact.depth": "1"})
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	impact := decodeArtifact[ImpactBundle](t, store, result, "impact.digest")
	if impact.Depth != 1 {
		t.Errorf("impact depth = %d, want 1", impact.Depth)
	}
}

func TestAnalyzeBinaryDiff(t *testing.T) {
	workspace := t.TempDir()
	baseDir := filepath.Join(workspace, "base")
	headDir := filepath.Join(workspace, "head")
	binaryBefore := "before\x00binary-payload"
	binaryAfter := "after\x00binary-payload"
	writeTree(t, baseDir, map[string]string{
		"go.mod":   fixtureModule,
		"data.bin": binaryBefore,
	})
	writeTree(t, headDir, map[string]string{
		"go.mod":   fixtureModule,
		"data.bin": binaryAfter,
	})
	staged := map[string]engine.StagedInput{
		"base": stagedDir(baseDir, strings.Repeat("b", 64)),
		"head": stagedDir(headDir, strings.Repeat("c", 64)),
	}
	request, store := testRequest(t, staged, nil)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s message = %s", result.Outcome, result.Message)
	}
	diff := decodeArtifact[DiffBundle](t, store, result, "diff.digest")
	if len(diff.Files) != 1 {
		t.Fatalf("diff files = %+v", diff.Files)
	}
	if !diff.Files[0].Binary {
		t.Errorf("binary file was not classified as binary: %+v", diff.Files[0])
	}
	if diff.Files[0].BaseDigest == "" || diff.Files[0].HeadDigest == "" {
		t.Errorf("binary diff missing digests: %+v", diff.Files[0])
	}
	ir := decodeArtifact[IRBundle](t, store, result, "ir.digest")
	for _, change := range ir.Changes {
		if change.Path == "data.bin" && len(change.Symbols) != 0 {
			t.Errorf("binary change must not carry symbols: %+v", change)
		}
	}
}

func TestAnalyzeBaseOnly(t *testing.T) {
	baseDir, _ := fixtureSnapshots(t)
	request, store := testRequest(t, map[string]engine.StagedInput{
		"base": stagedDir(baseDir, strings.Repeat("d", 64)),
	}, nil)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s message = %s", result.Outcome, result.Message)
	}
	ir := decodeArtifact[IRBundle](t, store, result, "ir.digest")
	if !hasSymbol(ir, "example.com/fixture/pkg#Add") {
		t.Errorf("base-only analysis lost symbols")
	}
	if len(ir.Changes) != 0 {
		t.Errorf("base-only analysis must not report changes: %+v", ir.Changes)
	}
	if ir.BaseCommit != "" {
		t.Errorf("base commit = %q, want empty", ir.BaseCommit)
	}
	if ir.HeadCommit == "" {
		t.Errorf("single staged snapshot should populate head_commit")
	}
}
