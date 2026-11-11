package pluginengine

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/cas/memory"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/plugin"
)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

type fakeService struct {
	capabilities plugin.Capabilities
	execute      func(context.Context, plugin.StepRequest) (plugin.StepResponse, error)

	mu   sync.Mutex
	seen plugin.StepRequest
}

func (f *fakeService) Capabilities(context.Context) (plugin.Capabilities, error) {
	capabilities := f.capabilities
	if capabilities.PluginID == "" {
		capabilities.PluginID = "fake"
	}
	if capabilities.Version == "" {
		capabilities.Version = "9.9.9"
	}
	if len(capabilities.Languages) == 0 {
		capabilities.Languages = []string{"go"}
	}
	if len(capabilities.StepKinds) == 0 {
		capabilities.StepKinds = []string{string(domain.StepKindAnalyze)}
	}
	return capabilities, nil
}

func (f *fakeService) Execute(ctx context.Context, request plugin.StepRequest) (plugin.StepResponse, error) {
	f.mu.Lock()
	f.seen = request
	f.mu.Unlock()
	if f.execute == nil {
		return plugin.StepResponse{
			AttemptID:         request.AttemptID,
			FencingGeneration: request.FencingGeneration,
			Outcome:           "SUCCEEDED",
		}, nil
	}
	response, err := f.execute(ctx, request)
	if err != nil {
		return plugin.StepResponse{}, err
	}
	if response.AttemptID == "" {
		response.AttemptID = request.AttemptID
	}
	if response.FencingGeneration == 0 {
		response.FencingGeneration = request.FencingGeneration
	}
	return response, nil
}

func (f *fakeService) request() plugin.StepRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

// newPipeDialer returns a Dialer that starts a Server on one end of a net.Pipe
// for every Execute. The client side is handed to the engine.
func newPipeDialer(t *testing.T, service plugin.Service) func(context.Context) (*plugin.Client, error) {
	t.Helper()
	var mu sync.Mutex
	var cleanup []func()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, stop := range cleanup {
			stop()
		}
	})
	return func(ctx context.Context) (*plugin.Client, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		serverConn, clientConn := net.Pipe()
		serverCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = (&plugin.Server{Service: service, MaxFrameBytes: 1 << 20}).Serve(serverCtx, serverConn)
		}()
		mu.Lock()
		cleanup = append(cleanup, func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("plugin server did not stop")
			}
		})
		mu.Unlock()
		return plugin.NewClient(clientConn, 1<<20), nil
	}
}

func testConfig(t *testing.T, service plugin.Service) Config {
	t.Helper()
	return Config{
		Name:          "fake-plugin",
		Version:       "0.1.0",
		Command:       []string{"/opt/patchproof/fake-plugin"},
		Languages:     []string{"go"},
		Kinds:         []domain.StepKind{domain.StepKindAnalyze},
		MaxFrameBytes: 1 << 20,
		Timeout:       10 * time.Second,
		Dialer:        newPipeDialer(t, service),
	}
}

func testRequest(t *testing.T, store ports.ArtifactStore) engine.Request {
	t.Helper()
	workspace := t.TempDir()
	tenant := domain.NewTenantID()
	return engine.Request{
		Spec: domain.StepSpec{
			RunID:        domain.NewRunID(),
			StepID:       domain.NewStepID(),
			AttemptID:    domain.NewAttemptID(),
			TenantID:     tenant,
			Generation:   3,
			Kind:         domain.StepKindAnalyze,
			Language:     "go",
			Engine:       "fake-plugin",
			PolicyDigest: "sha256:" + strings.Repeat("b", 64),
			Options:      map[string]string{"claim.id": "compile"},
			Deadline:     time.Now().Add(time.Minute),
			Requirements: domain.ResourceRequirements{
				Region:          "local",
				Architecture:    "amd64",
				Isolation:       domain.IsolationContainer,
				CPUMilli:        1000,
				MemoryMiB:       512,
				Network:         domain.NetworkNone,
				DeadlineSeconds: 60,
			},
		},
		TenantID:  tenant,
		Workspace: workspace,
		Staged: map[string]engine.StagedInput{
			"base": {
				Name: "base",
				Path: filepath.Join(workspace, "inputs", "base"),
				Ref:  domain.NewCASRef(strings.Repeat("a", 64), 3),
				Kind: "file",
			},
		},
		Store: store,
		Scope: ports.SecurityScope{TenantID: tenant, TrustDomain: "run", Region: "local"},
		Clock: fixedClock{},
	}
}

func TestExecuteUploadsFilesAndMapsDrafts(t *testing.T) {
	service := &fakeService{execute: func(_ context.Context, request plugin.StepRequest) (plugin.StepResponse, error) {
		if err := os.MkdirAll(filepath.Join(request.OutputDir, "observations"), 0o700); err != nil {
			return plugin.StepResponse{}, err
		}
		if err := os.WriteFile(filepath.Join(request.OutputDir, "report.json"), []byte(`{"ok":true}`), 0o600); err != nil {
			return plugin.StepResponse{}, err
		}
		if err := os.WriteFile(filepath.Join(request.OutputDir, "observations", "diff.txt"), []byte("diff"), 0o600); err != nil {
			return plugin.StepResponse{}, err
		}
		return plugin.StepResponse{
			Outcome: "SUCCEEDED",
			Files: []plugin.FileArtifact{
				{Name: "report", Path: "report.json", MediaType: "application/json"},
				{Name: "diff", Path: "observations/diff.txt", MediaType: "text/plain"},
			},
			EvidenceDrafts: []plugin.EvidenceDraft{{
				ClaimID: "compile",
				Kind:    "COMPILE",
				Result:  "TESTED_WITHIN_SCOPE",
				Scope: plugin.EvidenceScope{
					InputDomain:    "go",
					SeedCount:      4,
					GeneratedCases: 17,
					ElapsedMS:      31,
					Budget:         "small",
					Details:        map[string]string{"toolchain": "go1.26"},
				},
				Assumptions:   []string{"module graph is complete"},
				Limits:        []string{"linux/amd64 only"},
				ArtifactNames: []string{"report"},
				InputNames:    []string{"base"},
			}},
			CounterexampleDrafts: []plugin.CounterexampleDraft{{
				CaseID:              "case-1",
				ClaimID:             "compile",
				Scope:               "package example.com/fixture",
				InputCodec:          "json",
				InputFile:           "diff",
				BaseObservationFile: "report",
				HeadObservationFile: "diff",
				Reproduced:          true,
				Minimized:           true,
				Nondeterminism:      []string{"clock"},
			}},
			Metrics: &plugin.StepMetrics{CPUSeconds: 1.5, ExecutionMS: 20},
			Outputs: map[string]string{"engine": "fake"},
		}, nil
	}}

	store := memory.New()
	request := testRequest(t, store)
	expectedDir := filepath.Join(request.Workspace, ".patchproof", "plugin",
		sanitize(request.Spec.StepID.String()+"-"+request.Spec.AttemptID.String()))
	if err := os.MkdirAll(expectedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(expectedDir, "stale.txt"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter, err := New(testConfig(t, service))
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.OutcomeSucceeded || result.ErrorClass != "" || result.Retryable {
		t.Fatalf("unexpected outcome: %#v", result)
	}
	if result.Metrics.CPUSeconds != 1.5 || result.Metrics.ExecutionMS != 20 {
		t.Fatalf("metrics were not mapped: %#v", result.Metrics)
	}
	if result.Outputs["engine"] != "fake" {
		t.Fatalf("outputs were not mapped: %#v", result.Outputs)
	}
	if len(result.Artifacts) != 2 {
		t.Fatalf("artifacts = %#v", result.Artifacts)
	}

	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %#v", result.Evidence)
	}
	evidence := result.Evidence[0]
	if err := evidence.Validate(); err != nil {
		t.Fatalf("evidence is invalid: %v", err)
	}
	if evidence.SchemaVersion != domain.EvidenceSchemaV1 || evidence.ID == "" {
		t.Fatalf("evidence identity was not filled in: %#v", evidence)
	}
	if evidence.TenantID != request.Spec.TenantID || evidence.RunID != request.Spec.RunID || evidence.StepID != request.Spec.StepID {
		t.Fatalf("evidence identity does not match the request: %#v", evidence)
	}
	if evidence.Engine.Name != "fake-plugin" || evidence.Engine.Version != "9.9.9" || evidence.Engine.Protocol != Protocol {
		t.Fatalf("engine metadata was not derived: %#v", evidence.Engine)
	}
	if evidence.Environment.SandboxProfile != string(domain.IsolationContainer) || evidence.Environment.Region != "local" {
		t.Fatalf("environment was not derived: %#v", evidence.Environment)
	}
	if evidence.Scope.InputDomain != "go" || evidence.Scope.SeedCount != 4 ||
		evidence.Scope.GeneratedCases != 17 || evidence.Scope.Details["toolchain"] != "go1.26" {
		t.Fatalf("evidence scope was not mapped: %#v", evidence.Scope)
	}
	if len(evidence.Artifacts) != 1 || evidence.Artifacts[0] != result.Artifacts[0] {
		t.Fatalf("evidence artifacts were not resolved: %#v", evidence.Artifacts)
	}
	if len(evidence.Inputs) != 1 || evidence.Inputs[0] != request.Staged["base"].Ref {
		t.Fatalf("evidence inputs were not resolved: %#v", evidence.Inputs)
	}
	if evidence.CreatedAt != (fixedClock{}).Now() {
		t.Fatalf("evidence createdAt = %v", evidence.CreatedAt)
	}

	if len(result.Counterexamples) != 1 {
		t.Fatalf("counterexamples = %#v", result.Counterexamples)
	}
	counterexample := result.Counterexamples[0]
	if err := counterexample.Validate(); err != nil {
		t.Fatalf("counterexample is invalid: %v", err)
	}
	if counterexample.TenantID != request.Spec.TenantID || counterexample.RunID != request.Spec.RunID ||
		counterexample.ClaimID != "compile" || !counterexample.Reproduced || !counterexample.Minimized {
		t.Fatalf("counterexample metadata was not mapped: %#v", counterexample)
	}
	if counterexample.InputRef != result.Artifacts[1] ||
		counterexample.BaseObservationRef != result.Artifacts[0] ||
		counterexample.HeadObservationRef != result.Artifacts[1] {
		t.Fatalf("counterexample refs were not resolved: %#v", counterexample)
	}

	if content := readBlob(t, store, request.Scope, evidence.Artifacts[0]); content != `{"ok":true}` {
		t.Fatalf("uploaded report content = %q", content)
	}
	if content := readBlob(t, store, request.Scope, result.Artifacts[1]); content != "diff" {
		t.Fatalf("uploaded diff content = %q", content)
	}

	seen := service.request()
	if seen.Workspace != request.Workspace || seen.OutputDir != expectedDir {
		t.Fatalf("workspace/output dir were not passed: %#v", seen)
	}
	if seen.FencingGeneration != 3 || seen.PolicyDigest != request.Spec.PolicyDigest ||
		seen.StepKind != "analyze" || seen.Language != "go" || seen.Engine != "fake-plugin" {
		t.Fatalf("request metadata was not passed: %#v", seen)
	}
	if seen.Options["claim.id"] != "compile" || seen.Deadline.IsZero() {
		t.Fatalf("options/deadline were not passed: %#v", seen)
	}
	if staged := seen.StagedInputs["base"]; staged.Path != request.Staged["base"].Path || staged.Kind != "file" || staged.Ref.Digest != request.Staged["base"].Ref.Digest {
		t.Fatalf("staged input was not passed: %#v", staged)
	}
	if _, err := os.Stat(filepath.Join(expectedDir, "stale.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale output was not removed: %v", err)
	}
	info, err := os.Stat(expectedDir)
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o700 {
		t.Fatalf("output dir permission = %o, want 700", permission)
	}
}

func TestExecuteRejectsUnsafeOutputPaths(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "traversal", path: "../../escape"},
		{name: "nested traversal", path: "observations/../../escape"},
		{name: "absolute", path: "/etc/passwd"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakeService{execute: func(context.Context, plugin.StepRequest) (plugin.StepResponse, error) {
				return plugin.StepResponse{
					Outcome: "SUCCEEDED",
					Files:   []plugin.FileArtifact{{Name: "escape", Path: testCase.path}},
				}, nil
			}}
			adapter, err := New(testConfig(t, service))
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Execute(context.Background(), testRequest(t, memory.New()))
			if err == nil {
				t.Fatal("expected the output path to be rejected")
			}
			if !strings.Contains(err.Error(), "file artifact") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestExecuteRejectsSymlinkOutput(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &fakeService{execute: func(_ context.Context, request plugin.StepRequest) (plugin.StepResponse, error) {
		if err := os.Symlink(target, filepath.Join(request.OutputDir, "link.txt")); err != nil {
			return plugin.StepResponse{}, err
		}
		return plugin.StepResponse{
			Outcome: "SUCCEEDED",
			Files:   []plugin.FileArtifact{{Name: "link", Path: "link.txt"}},
		}, nil
	}}
	adapter, err := New(testConfig(t, service))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Execute(context.Background(), testRequest(t, memory.New()))
	if err == nil {
		t.Fatal("expected the symbolic link to be rejected")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteRejectsDuplicateFileNames(t *testing.T) {
	service := &fakeService{execute: func(_ context.Context, request plugin.StepRequest) (plugin.StepResponse, error) {
		if err := os.WriteFile(filepath.Join(request.OutputDir, "one.txt"), []byte("one"), 0o600); err != nil {
			return plugin.StepResponse{}, err
		}
		return plugin.StepResponse{
			Outcome: "SUCCEEDED",
			Files: []plugin.FileArtifact{
				{Name: "same", Path: "one.txt"},
				{Name: "same", Path: "one.txt"},
			},
		}, nil
	}}
	adapter, err := New(testConfig(t, service))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Execute(context.Background(), testRequest(t, memory.New()))
	if err == nil {
		t.Fatal("expected duplicate names to be rejected")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteRejectsMissingDraftReferences(t *testing.T) {
	service := &fakeService{execute: func(context.Context, plugin.StepRequest) (plugin.StepResponse, error) {
		return plugin.StepResponse{
			Outcome: "SUCCEEDED",
			EvidenceDrafts: []plugin.EvidenceDraft{{
				ClaimID:       "compile",
				Kind:          "COMPILE",
				Result:        "TESTED_WITHIN_SCOPE",
				Scope:         plugin.EvidenceScope{InputDomain: "go"},
				ArtifactNames: []string{"missing"},
			}},
		}, nil
	}}
	adapter, err := New(testConfig(t, service))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Execute(context.Background(), testRequest(t, memory.New()))
	if err == nil {
		t.Fatal("expected the unresolved artifact to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown file artifact") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteRejectsInvalidDraftEnums(t *testing.T) {
	cases := []struct {
		name  string
		draft plugin.EvidenceDraft
		want  string
	}{
		{
			name: "unknown evidence kind",
			draft: plugin.EvidenceDraft{
				ClaimID: "compile", Kind: "NOT_A_KIND", Result: "TESTED_WITHIN_SCOPE",
				Scope: plugin.EvidenceScope{InputDomain: "go"},
			},
			want: "evidence kind",
		},
		{
			name: "unknown claim result",
			draft: plugin.EvidenceDraft{
				ClaimID: "compile", Kind: "COMPILE", Result: "NOT_A_RESULT",
				Scope: plugin.EvidenceScope{InputDomain: "go"},
			},
			want: "claim result",
		},
		{
			name: "missing claim id",
			draft: plugin.EvidenceDraft{
				Kind: "COMPILE", Result: "TESTED_WITHIN_SCOPE",
				Scope: plugin.EvidenceScope{InputDomain: "go"},
			},
			want: "claim_id",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			draft := testCase.draft
			service := &fakeService{execute: func(context.Context, plugin.StepRequest) (plugin.StepResponse, error) {
				return plugin.StepResponse{Outcome: "SUCCEEDED", EvidenceDrafts: []plugin.EvidenceDraft{draft}}, nil
			}}
			adapter, err := New(testConfig(t, service))
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Execute(context.Background(), testRequest(t, memory.New()))
			if err == nil {
				t.Fatal("expected the draft to be rejected")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

func TestExecuteRejectsIncompatibleProtocolMajor(t *testing.T) {
	service := &fakeService{
		capabilities: plugin.Capabilities{ProtocolMajor: 2},
		execute: func(context.Context, plugin.StepRequest) (plugin.StepResponse, error) {
			return plugin.StepResponse{Outcome: "SUCCEEDED"}, nil
		},
	}
	adapter, err := New(testConfig(t, service))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Execute(context.Background(), testRequest(t, memory.New()))
	if err == nil {
		t.Fatal("expected the protocol major to be rejected")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteRejectsUnknownKind(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		adapter, err := New(testConfig(t, &fakeService{}))
		if err != nil {
			t.Fatal(err)
		}
		request := testRequest(t, memory.New())
		request.Spec.Kind = domain.StepKindCompile
		_, err = adapter.Execute(context.Background(), request)
		if !errors.Is(err, domain.ErrUnsupported) {
			t.Fatalf("expected ErrUnsupported, got %v", err)
		}
	})
	t.Run("not advertised", func(t *testing.T) {
		service := &fakeService{capabilities: plugin.Capabilities{StepKinds: []string{string(domain.StepKindCompile)}}}
		adapter, err := New(testConfig(t, service))
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapter.Execute(context.Background(), testRequest(t, memory.New()))
		if !errors.Is(err, domain.ErrUnsupported) {
			t.Fatalf("expected ErrUnsupported, got %v", err)
		}
	})
}

func TestExecuteDefaultsEmptyOutcomeToFailed(t *testing.T) {
	service := &fakeService{execute: func(context.Context, plugin.StepRequest) (plugin.StepResponse, error) {
		return plugin.StepResponse{}, nil
	}}
	adapter, err := New(testConfig(t, service))
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Execute(context.Background(), testRequest(t, memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.OutcomeFailed {
		t.Fatalf("outcome = %q, want FAILED", result.Outcome)
	}
}

func readBlob(t *testing.T, store ports.ArtifactStore, scope ports.SecurityScope, ref domain.CASRef) string {
	t.Helper()
	reader, err := store.Open(context.Background(), scope, ref)
	if err != nil {
		t.Fatalf("open %s: %v", ref.URI(), err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read %s: %v", ref.URI(), err)
	}
	return string(data)
}
