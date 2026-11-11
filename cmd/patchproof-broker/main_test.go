package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/cas/localfs"
	casmemory "github.com/patchproof/patchproof/internal/adapters/cas/memory"
	"github.com/patchproof/patchproof/internal/broker"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/worker"
)

func testSpec(t *testing.T, source *casmemory.Store) (domain.StepSpec, domain.CASRef) {
	t.Helper()
	spec := domain.StepSpec{
		RunID:      domain.NewRunID(),
		StepID:     domain.NewStepID(),
		AttemptID:  domain.NewAttemptID(),
		TenantID:   domain.NewTenantID(),
		Generation: 2,
		Kind:       domain.StepKindCompile,
		Language:   "go",
		Engine:     "go-analyzer",
	}
	scope := worker.ScopeFor(spec)
	ref, err := source.Put(context.Background(), scope, strings.NewReader("stage-in-content"))
	if err != nil {
		t.Fatalf("put source blob: %v", err)
	}
	spec.NamedInputs = map[string]domain.InputBinding{
		"snapshot": {Ref: ref, Kind: "tar.gz"},
	}
	return spec, ref
}

func writeSpecFile(t *testing.T, spec domain.StepSpec) string {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	path := filepath.Join(t.TempDir(), "step.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseOptions(t *testing.T) {
	opts, err := parseOptions([]string{
		"--phase=stage-in",
		"--spec-file=/run/spec/step.json",
		"--local-root=/work",
		"--attempt-token-file=/run/secrets/token",
		"--callback-url=https://controller.test",
	})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.phase != phaseStageIn || opts.specFile != "/run/spec/step.json" || opts.localRoot != "/work" {
		t.Fatalf("opts = %+v", opts)
	}
	if opts.attemptTokenFile != "/run/secrets/token" || opts.callbackURL != "https://controller.test" {
		t.Fatalf("opts = %+v", opts)
	}

	// Environment fallbacks are used when flags are omitted.
	t.Setenv("PATCHPROOF_SPEC_FILE", "/env/step.json")
	t.Setenv("PATCHPROOF_LOCAL_ROOT", "/env/work")
	t.Setenv("PATCHPROOF_CALLBACK_URL", "https://env-controller.test")
	opts, err = parseOptions([]string{"--phase", "stage-out"})
	if err != nil {
		t.Fatalf("parseOptions env fallback: %v", err)
	}
	if opts.specFile != "/env/step.json" || opts.localRoot != "/env/work" || opts.callbackURL != "https://env-controller.test" {
		t.Fatalf("env fallback opts = %+v", opts)
	}
	t.Setenv("PATCHPROOF_SPEC_FILE", "")
	t.Setenv("PATCHPROOF_LOCAL_ROOT", "")
	t.Setenv("PATCHPROOF_CALLBACK_URL", "")

	for name, args := range map[string][]string{
		"missing phase":           {"--spec-file=/x", "--local-root=/y"},
		"unknown phase":           {"--phase=sideways", "--spec-file=/x", "--local-root=/y"},
		"missing spec file":       {"--phase=stage-in", "--local-root=/y"},
		"missing local root":      {"--phase=stage-in", "--spec-file=/x"},
		"stage-out no callback":   {"--phase=stage-out", "--spec-file=/x", "--local-root=/y"},
		"unexpected positional":   {"--phase=stage-in", "--spec-file=/x", "--local-root=/y", "extra"},
		"unknown flag":            {"--phase=stage-in", "--spec-file=/x", "--local-root=/y", "--bogus=1"},
		"stage-in no callback ok": {"--phase=stage-in", "--spec-file=/x", "--local-root=/y"},
	} {
		_, err := parseOptions(args)
		switch name {
		case "stage-in no callback ok":
			if err != nil {
				t.Fatalf("%s: unexpected error %v", name, err)
			}
		default:
			if err == nil {
				t.Fatalf("%s: expected error", name)
			}
		}
	}
}

func TestLoadConfigDefaultsWorkerRole(t *testing.T) {
	t.Setenv("PATCHPROOF_ROLE", "")
	t.Setenv("PATCHPROOF_CONFIG", "")
	t.Setenv("PATCHPROOF_CAS_DRIVER", "memory")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Service.Role != config.RoleWorker {
		t.Fatalf("role = %q, want worker", cfg.Service.Role)
	}
	if cfg.Artifacts.Driver != "memory" {
		t.Fatalf("artifacts driver = %q, want memory", cfg.Artifacts.Driver)
	}
}

func TestRunStageInAgainstMemoryCAS(t *testing.T) {
	source := casmemory.New()
	spec, ref := testSpec(t, source)
	root := t.TempDir()
	opts := options{
		phase:     phaseStageIn,
		specFile:  writeSpecFile(t, spec),
		localRoot: root,
	}
	if err := execute(context.Background(), opts, source, testLogger()); err != nil {
		t.Fatalf("execute stage-in: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, broker.InputManifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest broker.InputManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Inputs["snapshot"] != ref {
		t.Fatalf("manifest = %+v", manifest)
	}

	local, err := localfs.New(root)
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	if _, err := local.Stat(context.Background(), worker.ScopeFor(spec), ref); err != nil {
		t.Fatalf("stat staged blob: %v", err)
	}
}

func TestRunStageOutCallback(t *testing.T) {
	var (
		mu       sync.Mutex
		calls    []string
		bodies   = map[string]map[string]any{}
		tokenSet []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		decoded := map[string]any{}
		_ = json.Unmarshal(body, &decoded)
		mu.Lock()
		calls = append(calls, r.URL.Path)
		bodies[r.URL.Path] = decoded
		tokenSet = append(tokenSet, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	source := casmemory.New()
	spec, _ := testSpec(t, source)
	root := t.TempDir()
	if _, err := broker.StageIn(context.Background(), spec, source, root, worker.ScopeFor(spec)); err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	result := domain.StepResult{
		Outcome:      domain.OutcomeSucceeded,
		ResultDigest: strings.Repeat("cd", 32),
		FinishedAt:   time.Now().UTC(),
	}
	if err := broker.WriteResult(root, broker.AttemptRefOf(spec), result, nil); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	tokenPath := filepath.Join(t.TempDir(), "attempt-token")
	if err := os.WriteFile(tokenPath, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	opts := options{
		phase:            phaseStageOut,
		specFile:         writeSpecFile(t, spec),
		localRoot:        root,
		attemptTokenFile: tokenPath,
		callbackURL:      server.URL,
	}
	target := casmemory.New()
	if err := execute(context.Background(), opts, target, testLogger()); err != nil {
		t.Fatalf("execute stage-out: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	attemptID := spec.AttemptID.String()
	startPath := "/internal/v1/attempts/" + attemptID + ":start"
	completePath := "/internal/v1/attempts/" + attemptID + ":complete"
	if len(calls) != 2 || calls[0] != startPath || calls[1] != completePath {
		t.Fatalf("calls = %v", calls)
	}
	if bodies[completePath]["result"] == nil {
		t.Fatalf("complete body = %+v", bodies[completePath])
	}
	for _, header := range tokenSet {
		if header != "Bearer secret-token" {
			t.Fatalf("authorization = %q, want bearer token", header)
		}
	}
	if _, err := target.Stat(context.Background(), worker.ScopeFor(spec), spec.NamedInputs["snapshot"].Ref); err != nil {
		t.Fatalf("staged blob was not uploaded: %v", err)
	}
}

func TestRunStageOutFailureCallback(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
		body  map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":fail") {
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			decoded := map[string]any{}
			_ = json.Unmarshal(raw, &decoded)
			mu.Lock()
			body = decoded
			mu.Unlock()
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	source := casmemory.New()
	spec, _ := testSpec(t, source)
	root := t.TempDir()
	if _, err := broker.StageIn(context.Background(), spec, source, root, worker.ScopeFor(spec)); err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	// Intentionally do not write result.json: stage-out must report Fail, not
	// Complete, even though the input upload succeeded.
	opts := options{
		phase:       phaseStageOut,
		specFile:    writeSpecFile(t, spec),
		localRoot:   root,
		callbackURL: server.URL,
	}
	t.Setenv("PATCHPROOF_ATTEMPT_TOKEN", "env-token")
	err := execute(context.Background(), opts, casmemory.New(), testLogger())
	if err == nil {
		t.Fatal("expected stage-out to fail without result.json")
	}
	mu.Lock()
	defer mu.Unlock()
	attemptID := spec.AttemptID.String()
	want := []string{
		"/internal/v1/attempts/" + attemptID + ":start",
		"/internal/v1/attempts/" + attemptID + ":fail",
	}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if body["error_class"] != string(domain.ErrValidationFailed) {
		t.Fatalf("fail body = %+v, want VALIDATION_FAILED", body)
	}
}

func TestRunStageOutMissingToken(t *testing.T) {
	t.Setenv("PATCHPROOF_ATTEMPT_TOKEN", "")
	opts := options{
		phase:       phaseStageOut,
		specFile:    writeSpecFile(t, testSpecOnly()),
		localRoot:   t.TempDir(),
		callbackURL: "https://controller.test",
	}
	err := execute(context.Background(), opts, casmemory.New(), testLogger())
	if err == nil || !strings.Contains(err.Error(), "attempt-token") {
		t.Fatalf("error = %v, want missing token error", err)
	}
}

func testSpecOnly() domain.StepSpec {
	return domain.StepSpec{
		RunID: domain.NewRunID(), StepID: domain.NewStepID(),
		AttemptID: domain.NewAttemptID(), TenantID: domain.NewTenantID(), Generation: 1,
	}
}

func TestExecuteRejectsUnsupportedPhase(t *testing.T) {
	opts := options{phase: "nope", specFile: writeSpecFile(t, testSpecOnly()), localRoot: t.TempDir()}
	if err := execute(context.Background(), opts, casmemory.New(), testLogger()); err == nil {
		t.Fatal("expected unsupported phase error")
	}
}

func TestReadSpecRejectsBadDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.json")
	for _, content := range []string{`{`, `[]`, `"string"`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write spec: %v", err)
		}
		if _, err := readSpec(path); err == nil {
			t.Fatalf("expected decode error for %q", content)
		}
	}
	if _, err := readSpec(filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing spec error = %v, want ErrNotExist", err)
	}
}
