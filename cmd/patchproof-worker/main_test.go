package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patchproof/patchproof/internal/adapters/cas/localfs"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/internal/worker"
)

func TestOpenArtifactStoreUsesLocalCASInOfflineMode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	cfg := config.Default()
	store, err := openArtifactStore(context.Background(), cfg, root)
	if err != nil {
		t.Fatalf("openArtifactStore: %v", err)
	}
	local, ok := store.(*localfs.Store)
	if !ok {
		t.Fatalf("store = %T, want *localfs.Store", store)
	}
	if local.Root() != root {
		t.Fatalf("root = %q, want %q", local.Root(), root)
	}
	spec := domain.StepSpec{
		RunID: domain.NewRunID(), StepID: domain.NewStepID(),
		AttemptID: domain.NewAttemptID(), TenantID: domain.NewTenantID(), Generation: 1,
	}
	scope := worker.ScopeFor(spec)
	ref, err := local.Put(context.Background(), scope, strings.NewReader("offline"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cas")); err != nil {
		t.Fatalf("expected local cas tree: %v", err)
	}
	if _, err := local.Stat(context.Background(), scope, ref); err != nil {
		t.Fatalf("stat: %v", err)
	}
}

func TestOpenArtifactStoreKeepsRemoteOnline(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Artifacts.Driver = "fs"
	cfg.Artifacts.Root = root
	store, err := openArtifactStore(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("openArtifactStore: %v", err)
	}
	if _, ok := store.(*localfs.Store); ok {
		t.Fatal("online mode must not use the attempt-local CAS")
	}
	var _ ports.ArtifactStore = store
}
