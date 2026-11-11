package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

type fakeService struct {
	executeErr error
	seen       StepRequest
}

func (f *fakeService) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{PluginID: "fake", Version: "1.0.0", Languages: []string{"go"}, StepKinds: []string{"analyze"}}, nil
}

func (f *fakeService) Execute(_ context.Context, request StepRequest) (StepResponse, error) {
	f.seen = request
	if f.executeErr != nil {
		return StepResponse{}, f.executeErr
	}
	evidence, _ := json.Marshal(map[string]string{"claim": "compile"})
	return StepResponse{
		AttemptID: request.AttemptID, FencingGeneration: request.FencingGeneration,
		Outcome: "SUCCEEDED", ResultDigest: "sha256:" + "aa", Evidence: []json.RawMessage{evidence},
		Outputs: map[string]string{"engine": "fake"},
	}, nil
}

func startPair(t *testing.T, service Service) *Client {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	server := &Server{Service: service, MaxFrameBytes: 1 << 20}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, serverConn) }()
	client := NewClient(clientConn, 1<<20)
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})
	return client
}

func TestCapabilitiesAndExecute(t *testing.T) {
	service := &fakeService{}
	client := startPair(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.PluginID != "fake" || capabilities.ProtocolMajor != ProtocolMajor {
		t.Fatalf("unexpected capabilities: %#v", capabilities)
	}
	response, err := client.Execute(ctx, StepRequest{
		RunID: "run", StepID: "step", AttemptID: "attempt", FencingGeneration: 2,
		StepKind: "analyze", Language: "go", Inputs: map[string]CASRef{"base": {Algorithm: "sha256", Digest: "abc"}},
		Options: map[string]string{"claim.id": "compile"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Outcome != "SUCCEEDED" || response.AttemptID != "attempt" {
		t.Fatalf("unexpected response: %#v", response)
	}
	if service.seen.FencingGeneration != 2 || service.seen.Inputs["base"].Digest != "abc" {
		t.Fatalf("request was not delivered: %#v", service.seen)
	}
}

func TestCancelAfterSuccessfulCallKeepsConnectionUsable(t *testing.T) {
	client := startPair(t, &fakeService{})
	for i := 0; i < 500; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := client.Capabilities(ctx); err != nil {
			t.Fatalf("iteration %d: capabilities: %v", i, err)
		}
		cancel()
		if _, err := client.Execute(context.Background(), StepRequest{AttemptID: "attempt", FencingGeneration: 1}); err != nil {
			t.Fatalf("iteration %d: execute after cancellation: %v", i, err)
		}
	}
}

func TestRemoteErrorsAreReturned(t *testing.T) {
	client := startPair(t, &fakeService{executeErr: errors.New("engine crashed")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Execute(ctx, StepRequest{AttemptID: "attempt", FencingGeneration: 1})
	if err == nil {
		t.Fatal("expected an error")
	}
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "EXECUTION_FAILED" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFrameRoundTripAndLimits(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	frame := Frame{ID: "1", Type: FrameExecute, Payload: json.RawMessage(`{"step":"x"}`)}
	go func() { _ = WriteFrame(right, frame, 1024) }()
	decoded, err := ReadFrame(left, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "1" || decoded.Type != FrameExecute {
		t.Fatalf("unexpected frame: %#v", decoded)
	}
	go func() { _ = WriteFrame(right, frame, 1024) }()
	if _, err := ReadFrame(left, 2); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestCapabilityCompatibility(t *testing.T) {
	if !(Capabilities{ProtocolMajor: ProtocolMajor}).Compatible() {
		t.Fatal("current major should be compatible")
	}
	if (Capabilities{ProtocolMajor: ProtocolMajor + 1}).Compatible() {
		t.Fatal("future major must be rejected")
	}
}

type draftService struct {
	seen StepRequest
}

func (d *draftService) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{
		PluginID:  "drafts",
		Version:   "2.1.0",
		Languages: []string{"go"},
		StepKinds: []string{"analyze"},
	}, nil
}

func (d *draftService) Execute(_ context.Context, request StepRequest) (StepResponse, error) {
	d.seen = request
	return StepResponse{
		AttemptID:         request.AttemptID,
		FencingGeneration: request.FencingGeneration,
		Outcome:           "SUCCEEDED",
		Files: []FileArtifact{
			{Name: "report", Path: "report.json", MediaType: "application/json"},
			{Name: "diff", Path: "observations/diff.txt", MediaType: "text/plain"},
		},
		EvidenceDrafts: []EvidenceDraft{{
			ClaimID: "compile",
			Kind:    "COMPILE",
			Result:  "TESTED_WITHIN_SCOPE",
			Scope: EvidenceScope{
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
		CounterexampleDrafts: []CounterexampleDraft{{
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
		Metrics: &StepMetrics{CPUSeconds: 1.25, ExecutionMS: 12},
		Outputs: map[string]string{"confidence": "high"},
	}, nil
}

func TestDraftsAndFilesSurviveWire(t *testing.T) {
	service := &draftService{}
	client := startPair(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.ProtocolMinor != ProtocolMinor {
		t.Fatalf("protocol minor = %d, want %d", capabilities.ProtocolMinor, ProtocolMinor)
	}

	request := StepRequest{
		RunID: "run", StepID: "step", AttemptID: "attempt", FencingGeneration: 7,
		PolicyDigest: "sha256:policy", StepKind: "analyze", Language: "go", Engine: "drafts",
		Workspace: "/sandbox/workspace", OutputDir: "/sandbox/workspace/.patchproof/plugin/step-attempt",
		StagedInputs: map[string]StagedInput{
			"base": {
				Ref:  CASRef{Algorithm: "sha256", Digest: "0123456789abcdef", SizeBytes: 12},
				Path: "/sandbox/workspace/inputs/base", Kind: "file",
			},
		},
		Deadline: time.Unix(1_700_000_100, 0).UTC(),
	}
	response, err := client.Execute(ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	if service.seen.Workspace != request.Workspace || service.seen.OutputDir != request.OutputDir {
		t.Fatalf("workspace/output dir were not delivered: %#v", service.seen)
	}
	staged := service.seen.StagedInputs["base"]
	if staged.Path != request.StagedInputs["base"].Path || staged.Kind != "file" || staged.Ref.Digest != "0123456789abcdef" {
		t.Fatalf("staged input was not delivered: %#v", staged)
	}

	if len(response.Files) != 2 {
		t.Fatalf("files = %#v", response.Files)
	}
	if response.Files[1].Name != "diff" || response.Files[1].Path != "observations/diff.txt" || response.Files[1].MediaType != "text/plain" {
		t.Fatalf("file artifact did not survive the wire: %#v", response.Files[1])
	}
	if len(response.EvidenceDrafts) != 1 {
		t.Fatalf("evidence drafts = %#v", response.EvidenceDrafts)
	}
	draft := response.EvidenceDrafts[0]
	if draft.ClaimID != "compile" || draft.Kind != "COMPILE" || draft.Result != "TESTED_WITHIN_SCOPE" {
		t.Fatalf("evidence draft identity did not survive the wire: %#v", draft)
	}
	if draft.Scope.InputDomain != "go" || draft.Scope.GeneratedCases != 17 || draft.Scope.Details["toolchain"] != "go1.26" {
		t.Fatalf("evidence scope did not survive the wire: %#v", draft.Scope)
	}
	if len(draft.Assumptions) != 1 || len(draft.Limits) != 1 || len(draft.ArtifactNames) != 1 || len(draft.InputNames) != 1 {
		t.Fatalf("evidence draft lists did not survive the wire: %#v", draft)
	}
	if len(response.CounterexampleDrafts) != 1 {
		t.Fatalf("counterexample drafts = %#v", response.CounterexampleDrafts)
	}
	counterexample := response.CounterexampleDrafts[0]
	if counterexample.CaseID != "case-1" || counterexample.InputFile != "diff" ||
		counterexample.BaseObservationFile != "report" || !counterexample.Reproduced ||
		!counterexample.Minimized || len(counterexample.Nondeterminism) != 1 {
		t.Fatalf("counterexample draft did not survive the wire: %#v", counterexample)
	}
	if response.Metrics == nil || response.Metrics.CPUSeconds != 1.25 || response.Metrics.ExecutionMS != 12 {
		t.Fatalf("metrics did not survive the wire: %#v", response.Metrics)
	}
	if response.Outputs["confidence"] != "high" {
		t.Fatalf("outputs did not survive the wire: %#v", response.Outputs)
	}
}
