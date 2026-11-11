package godifferential

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

func TestLoadReplayInput(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "replay.json")
	if err := os.WriteFile(path, []byte(`[1,{"a":2}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	request := engine.Request{
		Workspace: workspace,
		Spec:      domain.StepSpec{Options: map[string]string{stepopts.ReplayCase: "ce-abc"}},
		Staged:    map[string]engine.StagedInput{"replay": {Name: "replay", Path: path, Kind: "file"}},
	}
	replay, failure := loadReplayInput(request, workspace)
	if failure != nil {
		t.Fatalf("load replay: %+v", failure)
	}
	if replay == nil || replay.caseID != "ce-abc" {
		t.Fatalf("replay = %+v", replay)
	}
	if string(replay.raw) != `[1,{"a":2}]` {
		t.Fatalf("raw = %s", replay.raw)
	}
}

func TestLoadReplayInputRejectsMalformedJSON(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "replay.json")
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	request := engine.Request{
		Workspace: workspace,
		Staged:    map[string]engine.StagedInput{"replay": {Name: "replay", Path: path, Kind: "file"}},
	}
	if _, failure := loadReplayInput(request, workspace); failure == nil {
		t.Fatal("malformed replay input should fail")
	}
}
