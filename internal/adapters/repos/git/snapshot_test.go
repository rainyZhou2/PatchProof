package git

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/cas/memory"
	"github.com/patchproof/patchproof/internal/domain"
	clockpkg "github.com/patchproof/patchproof/internal/platform/clock"
	"github.com/patchproof/patchproof/internal/ports"
)

type testRepository struct {
	dir  string
	home string
}

func newTestRepository(t *testing.T) *testRepository {
	t.Helper()
	root := t.TempDir()
	repository := &testRepository{
		dir:  filepath.Join(root, "repo"),
		home: filepath.Join(root, "home"),
	}
	if err := os.MkdirAll(repository.dir, 0o755); err != nil {
		t.Fatalf("create repository dir: %v", err)
	}
	if err := os.MkdirAll(repository.home, 0o755); err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	repository.git(t, "init", "--quiet")
	return repository
}

func (r *testRepository) git(t *testing.T, args ...string) string {
	t.Helper()
	base := []string{
		"-c", "user.name=PatchProof Test",
		"-c", "user.email=test@patchproof.example",
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
		"-c", "init.defaultBranch=main",
	}
	command := exec.Command("git", append(base, args...)...)
	command.Dir = r.dir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.home,
		"TMPDIR=" + os.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/true",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
		"TZ=UTC",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (r *testRepository) writeFile(t *testing.T, relative, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(r.dir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create dir for %s: %v", relative, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", relative, err)
	}
}

func (r *testRepository) commit(t *testing.T, message string) string {
	t.Helper()
	r.git(t, "add", "--all")
	r.git(t, "commit", "--quiet", "-m", message)
	return r.git(t, "rev-parse", "HEAD")
}

func (r *testRepository) uri() string {
	return (&url.URL{Scheme: "file", Path: r.dir}).String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestSnapshotter(t *testing.T, config Config) (*Snapshotter, *memory.Store) {
	t.Helper()
	store := memory.New()
	if config.WorkDir == "" {
		config.WorkDir = t.TempDir()
	}
	snapshotter, err := New(config, store, nil, discardLogger())
	if err != nil {
		t.Fatalf("New snapshotter: %v", err)
	}
	return snapshotter, store
}

func snapshotRequest(uri, commit string, tenant domain.TenantID) ports.SnapshotRequest {
	return ports.SnapshotRequest{
		Repository: domain.RepositoryRef{Provider: "local", URI: uri, Commit: commit},
		TenantID:   tenant,
	}
}

func fileDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestSnapshotLocalRepository(t *testing.T) {
	repository := newTestRepository(t)
	repository.writeFile(t, "README.md", "v1\n", 0o644)
	repository.writeFile(t, "src/main.go", "package main\n\nfunc main() {}\n", 0o644)
	repository.writeFile(t, "scripts/run.sh", "#!/bin/sh\necho v1\n", 0o755)
	firstCommit := repository.commit(t, "first")

	workDir := t.TempDir()
	fetchedAt := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	fakeClock := clockpkg.NewFake(fetchedAt)
	store := memory.New()
	snapshotter, err := New(Config{
		AllowFileRepositories: true,
		AllowFullFetch:        true,
		WorkDir:               workDir,
	}, store, fakeClock, discardLogger())
	if err != nil {
		t.Fatalf("New snapshotter: %v", err)
	}
	if err := snapshotter.Health(t.Context()); err != nil {
		t.Fatalf("Health: %v", err)
	}

	tenantID := domain.NewTenantID()
	request := snapshotRequest(repository.uri(), firstCommit, tenantID)
	result, err := snapshotter.Snapshot(t.Context(), request)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	type expectedFile struct {
		content    string
		mode       string
		executable bool
	}
	expected := map[string]expectedFile{
		"README.md":      {content: "v1\n", mode: "0644"},
		"src/main.go":    {content: "package main\n\nfunc main() {}\n", mode: "0644"},
		"scripts/run.sh": {content: "#!/bin/sh\necho v1\n", mode: "0755", executable: true},
	}
	var expectedBytes int64
	for _, file := range expected {
		expectedBytes += int64(len(file.content))
	}

	manifest := result.Manifest
	if manifest.Commit != firstCommit {
		t.Fatalf("manifest commit = %q, want %q", manifest.Commit, firstCommit)
	}
	if manifest.Repository != request.Repository.URI {
		t.Fatalf("manifest repository = %q, want %q", manifest.Repository, request.Repository.URI)
	}
	if manifest.FileCount != len(expected) || len(manifest.Files) != len(expected) {
		t.Fatalf("manifest file count = %d/%d, want %d", manifest.FileCount, len(manifest.Files), len(expected))
	}
	if manifest.TotalBytes != expectedBytes {
		t.Fatalf("manifest total bytes = %d, want %d", manifest.TotalBytes, expectedBytes)
	}
	if !manifest.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("manifest fetched_at = %v, want %v", manifest.FetchedAt, fetchedAt)
	}
	if !strings.HasPrefix(manifest.Digest, "sha256:") || len(strings.TrimPrefix(manifest.Digest, "sha256:")) != 64 {
		t.Fatalf("manifest digest = %q, want sha256:<64 hex characters>", manifest.Digest)
	}

	paths := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		paths = append(paths, file.Path)
		want, ok := expected[file.Path]
		if !ok {
			t.Fatalf("unexpected snapshot file %q", file.Path)
		}
		if file.Digest != fileDigest(want.content) {
			t.Fatalf("%s digest = %q, want %q", file.Path, file.Digest, fileDigest(want.content))
		}
		if file.Size != int64(len(want.content)) {
			t.Fatalf("%s size = %d, want %d", file.Path, file.Size, len(want.content))
		}
		if file.Mode != want.mode {
			t.Fatalf("%s mode = %q, want %q", file.Path, file.Mode, want.mode)
		}
		if file.Executable != want.executable {
			t.Fatalf("%s executable = %t, want %t", file.Path, file.Executable, want.executable)
		}
	}
	wantPaths := []string{"README.md", "scripts/run.sh", "src/main.go"}
	sort.Strings(wantPaths)
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("manifest paths = %#v, want %#v (sorted)", paths, wantPaths)
	}

	scope := ports.SecurityScope{TenantID: tenantID, TrustDomain: "run"}
	archiveContents := readArchive(t, store, scope, result.Archive)
	for name, want := range expected {
		content, ok := archiveContents[name]
		if !ok {
			t.Fatalf("archive is missing %q; entries = %#v", name, archiveContents)
		}
		if content != want.content {
			t.Fatalf("archive %q = %q, want %q", name, content, want.content)
		}
	}

	// Snapshotting the same commit again must produce an identical manifest.
	again, err := snapshotter.Snapshot(t.Context(), request)
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	if again.Manifest.Digest != manifest.Digest {
		t.Fatalf("manifest digest is not deterministic: %q vs %q", again.Manifest.Digest, manifest.Digest)
	}
	if !reflect.DeepEqual(again.Manifest.Files, manifest.Files) {
		t.Fatalf("manifest files are not deterministic:\n%+v\n%+v", again.Manifest.Files, manifest.Files)
	}
	if again.Archive.Digest != result.Archive.Digest {
		t.Fatalf("archive digest is not deterministic: %q vs %q", again.Archive.Digest, result.Archive.Digest)
	}

	// The second commit produces a different manifest.
	repository.writeFile(t, "README.md", "v2\n", 0o644)
	repository.writeFile(t, "docs/notes.txt", "release notes\n", 0o644)
	secondCommit := repository.commit(t, "second")
	second, err := snapshotter.Snapshot(t.Context(), snapshotRequest(repository.uri(), secondCommit, tenantID))
	if err != nil {
		t.Fatalf("Snapshot second commit: %v", err)
	}
	if second.Manifest.Commit != secondCommit {
		t.Fatalf("second manifest commit = %q, want %q", second.Manifest.Commit, secondCommit)
	}
	if second.Manifest.Digest == manifest.Digest {
		t.Fatal("second commit produced the same manifest digest as the first")
	}
	if second.Manifest.FileCount != 4 || len(second.Manifest.Files) != 4 {
		t.Fatalf("second manifest file count = %d/%d, want 4", second.Manifest.FileCount, len(second.Manifest.Files))
	}
	secondFiles := make(map[string]domain.SnapshotFile, len(second.Manifest.Files))
	for _, file := range second.Manifest.Files {
		secondFiles[file.Path] = file
	}
	if _, ok := secondFiles["docs/notes.txt"]; !ok {
		t.Fatalf("second manifest is missing docs/notes.txt: %#v", secondFiles)
	}
	if got := secondFiles["README.md"].Digest; got != fileDigest("v2\n") {
		t.Fatalf("second README digest = %q, want %q", got, fileDigest("v2\n"))
	}
}

func readArchive(t *testing.T, store ports.ArtifactStore, scope ports.SecurityScope, ref domain.CASRef) map[string]string {
	t.Helper()
	if err := ref.Validate(); err != nil {
		t.Fatalf("archive ref is invalid: %v", err)
	}
	if ref.SizeBytes <= 0 {
		t.Fatalf("archive size = %d, want > 0", ref.SizeBytes)
	}
	reader, err := store.Open(t.Context(), scope, ref)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		reader.Close()
		t.Fatalf("read archive: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open archive gzip: %v", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	files := make(map[string]string)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read archive tar: %v", err)
		}
		if !header.FileInfo().Mode().IsRegular() {
			continue
		}
		content, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatalf("read archive entry %q: %v", header.Name, err)
		}
		files[header.Name] = string(content)
	}
	if len(files) == 0 {
		t.Fatal("archive contains no regular files")
	}
	return files
}

func TestSnapshotRejectsFileRepositoryWhenDisabled(t *testing.T) {
	repository := newTestRepository(t)
	repository.writeFile(t, "README.md", "content\n", 0o644)
	commit := repository.commit(t, "only")
	snapshotter, _ := newTestSnapshotter(t, Config{AllowFileRepositories: false, AllowFullFetch: true})

	_, err := snapshotter.Snapshot(t.Context(), snapshotRequest(repository.uri(), commit, domain.NewTenantID()))
	if err == nil {
		t.Fatal("file repository was accepted while file repositories are disabled")
	}
	var validation *domain.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v (%T), want *domain.ValidationError", err, err)
	}
	if validation.Field != "repository_uri" || !strings.Contains(validation.Reason, "file repositories are not allowed") {
		t.Fatalf("validation error = %+v", validation)
	}
}

func TestSnapshotRejectsInvalidRepositories(t *testing.T) {
	tests := []struct {
		name      string
		allowFile bool
		uri       string
		wantField string
		want      string
	}{
		{
			name: "file disabled", allowFile: false, uri: "file:///tmp/repo",
			wantField: "repository_uri", want: "file repositories are not allowed",
		},
		{
			name: "unsupported scheme", allowFile: true, uri: "ftp://example.com/repo.git",
			wantField: "repository_uri", want: "unsupported scheme",
		},
		{
			name: "malformed uri", allowFile: true, uri: "://bad",
			wantField: "repository_uri", want: "not a valid URL",
		},
		{
			name: "http disabled", allowFile: true, uri: "http://example.com/repo.git",
			wantField: "repository_uri", want: "http is not allowed",
		},
		{
			name: "missing host", allowFile: true, uri: "https:///repo.git",
			wantField: "repository_uri", want: "missing host",
		},
		{
			name: "https credentials", allowFile: true, uri: "https://user:password@example.com/repo.git",
			wantField: "repository_uri", want: "embedded credentials are not allowed",
		},
		{
			name: "ssh credentials", allowFile: true, uri: "ssh://user:password@example.com/repo.git",
			wantField: "repository_uri", want: "embedded credentials are not allowed",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			snapshotter, _ := newTestSnapshotter(t, Config{
				AllowFileRepositories: test.allowFile,
				AllowFullFetch:        true,
			})
			_, err := snapshotter.Snapshot(t.Context(), snapshotRequest(test.uri, "deadbeef", domain.NewTenantID()))
			if err == nil {
				t.Fatalf("Snapshot accepted repository %q", test.uri)
			}
			var validation *domain.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error = %v (%T), want *domain.ValidationError", err, err)
			}
			if validation.Field != test.wantField || !strings.Contains(validation.Reason, test.want) {
				t.Fatalf("validation error = %+v, want field %q and reason containing %q", validation, test.wantField, test.want)
			}
		})
	}
}

func TestNewSnapshotterRequiresStore(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}, nil, nil, nil); err == nil {
		t.Fatal("expected nil store to be rejected")
	}
}
