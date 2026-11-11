package gobuild

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

func TestApplyImpactPatternsScopesRegression(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "impact.json")
	body := `{"schema_version":"patchproof.impact/v1","affected_packages":["example.com/mod/pkg","../escape","example.com/mod/pkg","example.com/mod/other"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	request := engine.Request{
		Workspace: workspace,
		Staged:    map[string]engine.StagedInput{"impact": {Name: "impact", Path: path, Kind: "file"}},
	}
	view, err := applyImpactPatterns(request, optionsView{patterns: []string{"./..."}}, domain.StepKindRegression)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"example.com/mod/pkg", "example.com/mod/other"}
	if !reflect.DeepEqual(view.patterns, want) {
		t.Fatalf("patterns = %v, want %v", view.patterns, want)
	}
	if !view.impactScoped {
		t.Fatal("impact scope was not recorded")
	}
}

func TestApplyImpactPatternsRespectsExplicitPatterns(t *testing.T) {
	view, err := applyImpactPatterns(engine.Request{}, optionsView{
		patterns: []string{"./cmd/..."}, patternsExplicit: true,
	}, domain.StepKindRegression)
	if err != nil {
		t.Fatal(err)
	}
	if view.impactScoped || !reflect.DeepEqual(view.patterns, []string{"./cmd/..."}) {
		t.Fatalf("explicit patterns were overridden: %+v", view)
	}
}

func TestApplyImpactPatternsRejectsMalformedInput(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "impact.json")
	if err := os.WriteFile(path, []byte(`not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	request := engine.Request{
		Workspace: workspace,
		Staged:    map[string]engine.StagedInput{"impact": {Name: "impact", Path: path, Kind: "file"}},
	}
	if _, err := applyImpactPatterns(request, optionsView{}, domain.StepKindRegression); err == nil {
		t.Fatal("malformed impact input should be rejected")
	}
}
