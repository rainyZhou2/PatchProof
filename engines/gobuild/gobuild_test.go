package gobuild

import (
	"bytes"
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

func TestCompileSuccess(t *testing.T) {
	root := moduleWith(t, map[string]string{
		"main.go": "package main\n\nfunc main() {}\n",
	})
	request, store := testRequest(t, domain.StepKindCompile, map[string]engine.StagedInput{"head": stagedDir(root)}, nil, time.Time{})
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s message = %s", result.Outcome, result.Message)
	}
	if result.ErrorClass != "" {
		t.Errorf("error class = %s", result.ErrorClass)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d", len(result.Evidence))
	}
	evidence := result.Evidence[0]
	if evidence.Kind != domain.EvidenceCompile || evidence.Result != domain.ClaimTested {
		t.Errorf("evidence kind/result = %s/%s", evidence.Kind, evidence.Result)
	}
	if evidence.ClaimID != "compile" {
		t.Errorf("default claim id = %s", evidence.ClaimID)
	}
	if evidence.Scope.InputDomain != "all packages matched by patterns" {
		t.Errorf("scope = %s", evidence.Scope.InputDomain)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("artifacts = %d", len(result.Artifacts))
	}
	logText := artifactText(t, store, result, "log.digest")
	if !strings.Contains(logText, "exit_code: 0") || !strings.Contains(logText, "target: head") {
		t.Errorf("unexpected log:\n%s", logText)
	}
	if err := evidence.Validate(); err != nil {
		t.Errorf("evidence invalid: %v", err)
	}
}

func TestCompileFailure(t *testing.T) {
	root := moduleWith(t, map[string]string{
		"main.go": "package main\n\nfunc main() {\n\tvar value int = \"not an int\"\n\t_ = value\n}\n",
	})
	request, store := testRequest(t, domain.StepKindCompile, map[string]engine.StagedInput{"head": stagedDir(root)}, nil, time.Time{})
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeFailed {
		t.Fatalf("outcome = %s, want FAILED", result.Outcome)
	}
	if result.ErrorClass != domain.ErrCompilationFailed {
		t.Errorf("error class = %s, want COMPILATION_FAILED", result.ErrorClass)
	}
	if result.Retryable {
		t.Errorf("compile failures must not be retryable")
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d", len(result.Evidence))
	}
	evidence := result.Evidence[0]
	if evidence.Kind != domain.EvidenceCompile || evidence.Result != domain.ClaimViolated {
		t.Errorf("evidence kind/result = %s/%s", evidence.Kind, evidence.Result)
	}
	if len(evidence.Artifacts) == 0 {
		t.Errorf("violated evidence must reference the compile log")
	}
	logText := artifactText(t, store, result, "log.digest")
	if !strings.Contains(logText, "exit_code: 1") {
		t.Errorf("log should record a non-zero exit code:\n%s", logText)
	}
}

func TestCompileBaseTarget(t *testing.T) {
	baseRoot := moduleWith(t, map[string]string{
		"main.go": "package main\n\nfunc main() {}\n",
	})
	headRoot := t.TempDir()
	writeTree(t, headRoot, map[string]string{
		"go.mod":  "module example.com/buildtest\n\ngo 1.21\n",
		"main.go": "package main\n\nfunc main() { broken }\n",
	})
	staged := map[string]engine.StagedInput{
		"base": {Name: "base", Path: baseRoot, Ref: domain.NewCASRef(strings.Repeat("a", 64), 1), Kind: "tar.gz"},
		"head": stagedDir(headRoot),
	}
	request, store := testRequest(t, domain.StepKindCompile, staged, map[string]string{"target": "base"}, time.Time{})
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s message = %s", result.Outcome, result.Message)
	}
	logText := artifactText(t, store, result, "log.digest")
	if !strings.Contains(logText, "target: base") {
		t.Errorf("log does not target base:\n%s", logText)
	}
}

func TestCompileMissingToolchain(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := moduleWith(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	request, _ := testRequest(t, domain.StepKindCompile, map[string]engine.StagedInput{"head": stagedDir(root)}, nil, time.Time{})
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeFailed || result.ErrorClass != domain.ErrSandboxUnavailable {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !strings.Contains(result.Message, "toolchain") {
		t.Errorf("message = %s", result.Message)
	}
	if result.Retryable {
		t.Errorf("missing toolchain must not be retryable")
	}
}

func TestCompileRejectsUnsafePatterns(t *testing.T) {
	root := moduleWith(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	for _, pattern := range []string{"../escape", "/abs/path", "./pkg;rm -rf /", "$(whoami)"} {
		request, _ := testRequest(t, domain.StepKindCompile, map[string]engine.StagedInput{"head": stagedDir(root)}, map[string]string{"go.patterns": pattern}, time.Time{})
		result, err := New().Execute(context.Background(), request)
		if err != nil {
			t.Fatalf("Execute(%q): %v", pattern, err)
		}
		if result.Outcome != domain.OutcomeFailed || result.ErrorClass != domain.ErrValidationFailed {
			t.Errorf("pattern %q: unexpected result %+v", pattern, result)
		}
	}
}

func TestRegressionPass(t *testing.T) {
	root := moduleWith(t, map[string]string{
		"fixture.go":      "package buildtest\n\nfunc Add(a, b int) int { return a + b }\n",
		"fixture_test.go": "package buildtest\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n",
	})
	request, store := testRequest(t, domain.StepKindRegression, map[string]engine.StagedInput{"head": stagedDir(root)}, map[string]string{"claim.id": "regression:unit"}, time.Time{})
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("outcome = %s message = %s", result.Outcome, result.Message)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d", len(result.Evidence))
	}
	evidence := result.Evidence[0]
	if evidence.Kind != domain.EvidenceRegression || evidence.Result != domain.ClaimTested {
		t.Errorf("evidence kind/result = %s/%s", evidence.Kind, evidence.Result)
	}
	if evidence.ClaimID != "regression:unit" {
		t.Errorf("claim id = %s", evidence.ClaimID)
	}
	bundle := decodeTestResults(t, store, result)
	if len(bundle.Packages) != 1 {
		t.Fatalf("packages = %d", len(bundle.Packages))
	}
	if bundle.Packages[0].Status != "pass" {
		t.Errorf("package status = %s", bundle.Packages[0].Status)
	}
	if len(bundle.Packages[0].Tests) != 1 || bundle.Packages[0].Tests[0].Name != "TestAdd" {
		t.Errorf("tests = %+v", bundle.Packages[0].Tests)
	}
	if bundle.Packages[0].Tests[0].Status != "pass" {
		t.Errorf("test status = %s", bundle.Packages[0].Tests[0].Status)
	}
	logText := artifactText(t, store, result, "log.digest")
	if !strings.Contains(logText, "exit_code: 0") {
		t.Errorf("log = %s", logText)
	}
}

func TestRegressionFailure(t *testing.T) {
	root := moduleWith(t, map[string]string{
		"fixture.go":      "package buildtest\n\nfunc Add(a, b int) int { return a + b }\n",
		"fixture_test.go": "package buildtest\n\nimport \"testing\"\n\nfunc TestBoom(t *testing.T) {\n\tt.Fatal(\"boom\")\n}\n",
	})
	request, store := testRequest(t, domain.StepKindRegression, map[string]engine.StagedInput{"head": stagedDir(root)}, nil, time.Time{})
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeFailed {
		t.Fatalf("outcome = %s", result.Outcome)
	}
	if result.ErrorClass != domain.ErrTestAssertionFailed {
		t.Errorf("error class = %s, want TEST_ASSERTION_FAILED", result.ErrorClass)
	}
	if result.Retryable {
		t.Errorf("regression failures must not be retryable")
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Result != domain.ClaimViolated {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	if result.Evidence[0].Kind != domain.EvidenceRegression {
		t.Errorf("evidence kind = %s", result.Evidence[0].Kind)
	}
	bundle := decodeTestResults(t, store, result)
	if len(bundle.Packages) != 1 || bundle.Packages[0].Status != "fail" {
		t.Fatalf("packages = %+v", bundle.Packages)
	}
	if len(bundle.Packages[0].Tests) != 1 || bundle.Packages[0].Tests[0].Status != "fail" {
		t.Fatalf("tests = %+v", bundle.Packages[0].Tests)
	}
	if !strings.Contains(bundle.Packages[0].Tests[0].Output, "boom") {
		t.Errorf("failure output missing: %q", bundle.Packages[0].Tests[0].Output)
	}
	if !strings.Contains(result.Message, "failed") {
		t.Errorf("message = %s", result.Message)
	}
}

func TestRegressionBuildFailure(t *testing.T) {
	root := moduleWith(t, map[string]string{
		"fixture.go": "package buildtest\n\nfunc Broken() int {\n\treturn missingValue\n}\n",
	})
	request, store := testRequest(t, domain.StepKindRegression, map[string]engine.StagedInput{"head": stagedDir(root)}, nil, time.Time{})
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeFailed || result.ErrorClass != domain.ErrCompilationFailed {
		t.Fatalf("unexpected result: outcome=%s class=%s message=%s", result.Outcome, result.ErrorClass, result.Message)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Result != domain.ClaimViolated {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	bundle := decodeTestResults(t, store, result)
	if len(bundle.Packages) == 0 || !bundle.Packages[0].BuildFailed {
		t.Errorf("bundle should record a build failure: %+v", bundle.Packages)
	}
	if !strings.Contains(result.Message, "build") {
		t.Errorf("message = %s", result.Message)
	}
}

func TestRegressionTimeout(t *testing.T) {
	root := moduleWith(t, map[string]string{
		"fixture_test.go": "package buildtest\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestSlow(t *testing.T) {\n\ttime.Sleep(2 * time.Second)\n}\n",
	})
	deadline := time.Now().Add(700 * time.Millisecond)
	request, _ := testRequest(t, domain.StepKindRegression, map[string]engine.StagedInput{"head": stagedDir(root)}, nil, deadline)
	result, err := New().Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != domain.OutcomeTimedOut {
		t.Fatalf("outcome = %s, want TIMED_OUT (message=%s)", result.Outcome, result.Message)
	}
	if result.ErrorClass != domain.ErrTimeout {
		t.Errorf("error class = %s", result.ErrorClass)
	}
	if result.Retryable {
		t.Errorf("timeout must not be retryable")
	}
	if len(result.Evidence) != 0 {
		t.Errorf("timeout must not emit evidence, got %d", len(result.Evidence))
	}
	if len(result.Artifacts) == 0 {
		t.Errorf("timeout should keep diagnostic artifacts")
	}
}

func TestRunGoCommandKillsChildProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are not available on windows")
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "orphan-ran")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	outcome := runGoCommand(ctx, "/bin/sh", directory, nil,
		[]string{"-c", `(sleep 2 && : > "$1") & sleep 30`, "sh", marker})
	if !outcome.timedOut {
		t.Fatalf("outcome = %+v, want timeout", outcome)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("background child survived cancellation and wrote %s", marker)
	}
}

func TestCappedBufferTruncates(t *testing.T) {
	buffer := newCappedBuffer(maxLogBytes)
	chunk := make([]byte, 1<<20)
	if _, err := buffer.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write(bytes.Repeat([]byte("x"), maxLogBytes)); err != nil {
		t.Fatal(err)
	}
	if !buffer.Truncated() {
		t.Errorf("buffer should be truncated")
	}
	if got := len(buffer.String()); got > maxLogBytes {
		t.Errorf("buffer length = %d, cap = %d", got, maxLogBytes)
	}
	small := newCappedBuffer(16)
	small.Write([]byte("short"))
	if small.Truncated() {
		t.Errorf("small buffer should not be truncated")
	}
}

func TestParseTestArgs(t *testing.T) {
	args, err := parseTestArgs("-run TestFoo -short")
	if err != nil {
		t.Fatalf("parseTestArgs: %v", err)
	}
	if len(args) != 3 || args[0] != "-run" || args[1] != "TestFoo" || args[2] != "-short" {
		t.Errorf("args = %v", args)
	}
	for _, raw := range []string{"-race", "-count=2", "-timeout 5s", "-exec wrapper", "-cover"} {
		if _, err := parseTestArgs(raw); err == nil {
			t.Errorf("expected %q to be rejected", raw)
		}
	}
	if args, err := parseTestArgs(""); err != nil || args != nil {
		t.Errorf("empty args = %v, %v", args, err)
	}
}

func TestStagedSnapshotRejectsSymlinkEscapeRoot(t *testing.T) {
	root := moduleWith(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	request, _ := testRequest(t, domain.StepKindCompile, map[string]engine.StagedInput{"head": stagedDir(link)}, nil, time.Time{})
	resolved, err := stagedSnapshot(request, "head")
	if err != nil {
		t.Fatalf("stagedSnapshot: %v", err)
	}
	if resolved == link {
		t.Errorf("stagedSnapshot should resolve symlinks")
	}
}

func TestBuildEnvironmentSanitized(t *testing.T) {
	workspace, err := prepareBuildWorkspace(requestForEnv(t))
	if err != nil {
		t.Fatalf("prepareBuildWorkspace: %v", err)
	}
	environment := buildEnvironment(workspace, optionsView{proxy: "off"})
	joined := strings.Join(environment, "\n")
	for _, key := range []string{"HOME=" + workspace.home, "GOCACHE=", "GOPATH=", "GOMODCACHE=", "GO111MODULE=on", "GOTOOLCHAIN=local", "GOPROXY=off", "CGO_ENABLED=0", "GOWORK=off"} {
		if !strings.Contains(joined, key) {
			t.Errorf("environment missing %s", key)
		}
	}
}

// requestForEnv builds a minimal request with a temporary workspace.
func requestForEnv(t *testing.T) engine.Request {
	t.Helper()
	return engine.Request{TenantID: testTenantID, Workspace: t.TempDir()}
}

func TestParseTestJSONAggregates(t *testing.T) {
	events := strings.Join([]string{
		`{"Action":"start","Package":"example.com/a"}`,
		`{"Action":"run","Package":"example.com/a","Test":"TestPass"}`,
		`{"Action":"output","Package":"example.com/a","Test":"TestPass","Output":"=== RUN   TestPass"}`,
		`{"Action":"pass","Package":"example.com/a","Test":"TestPass","Elapsed":0.01}`,
		`{"Action":"run","Package":"example.com/a","Test":"TestSkip"}`,
		`{"Action":"skip","Package":"example.com/a","Test":"TestSkip","Elapsed":0}`,
		`{"Action":"run","Package":"example.com/a","Test":"TestFail"}`,
		`{"Action":"output","Package":"example.com/a","Test":"TestFail","Output":"    boom failure"}`,
		`{"Action":"fail","Package":"example.com/a","Test":"TestFail","Elapsed":0.02}`,
		`{"Action":"fail","Package":"example.com/a","Elapsed":0.05}`,
		`{"Action":"output","Package":"example.com/b","Output":"FAIL\texample.com/b [build failed]"}`,
		`{"Action":"fail","Package":"example.com/b","Elapsed":0}`,
		`not json`,
	}, "\n")
	limits := []string{}
	bundle, sawEvents := parseTestJSON([]byte(events), &limits)
	if !sawEvents {
		t.Fatalf("expected events")
	}
	if len(bundle.Packages) != 2 {
		t.Fatalf("packages = %+v", bundle.Packages)
	}
	if bundle.Packages[0].Path != "example.com/a" || bundle.Packages[0].Status != "fail" {
		t.Errorf("package a = %+v", bundle.Packages[0])
	}
	if len(bundle.Packages[0].Tests) != 3 {
		t.Fatalf("tests = %+v", bundle.Packages[0].Tests)
	}
	statuses := map[string]string{}
	for _, test := range bundle.Packages[0].Tests {
		statuses[test.Name] = test.Status
	}
	if statuses["TestPass"] != "pass" || statuses["TestSkip"] != "skip" || statuses["TestFail"] != "fail" {
		t.Errorf("statuses = %+v", statuses)
	}
	for _, test := range bundle.Packages[0].Tests {
		if test.Name == "TestFail" && !strings.Contains(test.Output, "boom failure") {
			t.Errorf("failure output = %q", test.Output)
		}
	}
	if !bundle.Packages[1].BuildFailed {
		t.Errorf("package b should be marked as a build failure: %+v", bundle.Packages[1])
	}
	if !bundleFailed(bundle) || !bundleBuildFailed(bundle) {
		t.Errorf("bundle failure helpers disagree")
	}
	if len(bundle.Limits) == 0 {
		t.Errorf("expected a limitation for the non-JSON line")
	}
}

func TestTestTimeoutSeconds(t *testing.T) {
	if got := testTimeoutSeconds(time.Time{}); got != defaultTestTimeoutSeconds {
		t.Errorf("no deadline = %d", got)
	}
	if got := testTimeoutSeconds(time.Now().Add(30 * time.Second)); got <= 0 || got > 30 {
		t.Errorf("deadline budget = %d", got)
	}
	if got := testTimeoutSeconds(time.Now().Add(time.Second)); got != 1 {
		t.Errorf("tight deadline budget = %d", got)
	}
}
