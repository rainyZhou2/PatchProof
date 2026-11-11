package gosecurity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

func requestFor(t *testing.T, files map[string]string) engine.Request {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return engine.Request{
		Spec: domain.StepSpec{
			RunID: domain.NewRunID(), StepID: domain.NewStepID(), TenantID: domain.NewTenantID(),
			Options: map[string]string{stepopts.ClaimID: "security-scan"},
		},
		TenantID: domain.NewTenantID(),
		Staged:   map[string]engine.StagedInput{"head": {Path: root}},
	}
}

func TestSecurityScanCleanSource(t *testing.T) {
	result, err := New().Execute(context.Background(), requestFor(t, map[string]string{
		"main.go": "package main\n\nfunc main() {}\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.OutcomeSucceeded || len(result.Evidence) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Evidence[0].Result != domain.ClaimTested {
		t.Fatalf("evidence result = %s, want TESTED", result.Evidence[0].Result)
	}
}

func TestSecurityScanFindsInsecurePatterns(t *testing.T) {
	result, err := New().Execute(context.Background(), requestFor(t, map[string]string{
		"main.go": `package main

import (
	"crypto/tls"
	"os/exec"
)

func main() {
	_ = &tls.Config{InsecureSkipVerify: true}
	_ = exec.Command("sh", "-c", "echo hi")
}
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Result != domain.ClaimViolated {
		t.Fatalf("result = %+v", result)
	}
	if got := result.Evidence[0].Scope.Details["findings"]; got != "2" {
		t.Fatalf("findings = %q, want 2", got)
	}
}
