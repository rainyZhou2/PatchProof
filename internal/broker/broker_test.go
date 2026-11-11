package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/cas/localfs"
	casmemory "github.com/patchproof/patchproof/internal/adapters/cas/memory"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

func testSpec() domain.StepSpec {
	return domain.StepSpec{
		RunID:      domain.NewRunID(),
		StepID:     domain.NewStepID(),
		AttemptID:  domain.NewAttemptID(),
		TenantID:   domain.NewTenantID(),
		Generation: 4,
		Kind:       domain.StepKindCompile,
		Language:   "go",
		Engine:     "go-analyzer",
	}
}

func testScope(spec domain.StepSpec) ports.SecurityScope {
	return ports.SecurityScope{TenantID: spec.TenantID, TrustDomain: "run", Region: "local"}
}

func succeededResult(t *testing.T) domain.StepResult {
	t.Helper()
	return domain.StepResult{
		Outcome:      domain.OutcomeSucceeded,
		ResultDigest: strings.Repeat("ab", 32),
		Metrics:      domain.StepMetrics{ExecutionMS: 12},
		Outputs:      map[string]string{"build": "ok"},
		FinishedAt:   time.Now().UTC().Truncate(time.Second),
	}
}

func putBlob(t *testing.T, store ports.ArtifactStore, scope ports.SecurityScope, content string) domain.CASRef {
	t.Helper()
	ref, err := store.Put(context.Background(), scope, strings.NewReader(content))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	return ref
}

func TestStageInAndStageOutRoundtrip(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	snapshot := putBlob(t, source, scope, "snapshot-archive-bytes")
	config := putBlob(t, source, scope, "config-file-bytes")

	spec.NamedInputs = map[string]domain.InputBinding{
		"snapshot": {Ref: snapshot, Kind: "tar.gz"},
		"config":   {Ref: config, Kind: "file", Path: "configs/app.yaml"},
	}
	spec.Inputs = []domain.CASRef{config}

	root := t.TempDir()
	manifest, err := StageIn(context.Background(), spec, source, root, scope)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	if manifest.AttemptRef != AttemptRefOf(spec) {
		t.Fatalf("manifest ref = %+v, want %+v", manifest.AttemptRef, AttemptRefOf(spec))
	}
	if manifest.Inputs["snapshot"] != snapshot || manifest.Inputs["config"] != config || manifest.Inputs["input-0"] != config {
		t.Fatalf("manifest inputs = %+v", manifest.Inputs)
	}

	raw, err := os.ReadFile(filepath.Join(root, InputManifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var decoded InputManifest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(decoded.Inputs) != 3 || decoded.Inputs["snapshot"] != snapshot {
		t.Fatalf("decoded manifest = %+v", decoded)
	}

	local, err := localfs.New(root)
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	blobs, err := local.Blobs(context.Background())
	if err != nil {
		t.Fatalf("local blobs: %v", err)
	}
	if len(blobs) != 2 {
		t.Fatalf("local blobs = %d, want 2 (content-addressed dedupe)", len(blobs))
	}
	for name, ref := range map[string]domain.CASRef{"snapshot": snapshot, "config": config} {
		if _, err := local.Stat(context.Background(), scope, ref); err != nil {
			t.Fatalf("local stat %s: %v", name, err)
		}
	}

	result := succeededResult(t)
	if err := WriteResult(root, AttemptRefOf(spec), result, nil); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	document, err := ReadResult(root)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if document.Error != "" || document.AttemptRef != AttemptRefOf(spec) || document.Result.Outcome != domain.OutcomeSucceeded {
		t.Fatalf("document = %+v", document)
	}
	if document.Result.ResultDigest != result.ResultDigest {
		t.Fatalf("result digest = %q, want %q", document.Result.ResultDigest, result.ResultDigest)
	}

	target := casmemory.New()
	uploaded, err := UploadBlobs(context.Background(), root, target, scope)
	if err != nil {
		t.Fatalf("UploadBlobs: %v", err)
	}
	if len(uploaded) != 2 {
		t.Fatalf("uploaded = %d, want 2", len(uploaded))
	}
	for _, ref := range []domain.CASRef{snapshot, config} {
		if _, err := target.Stat(context.Background(), scope, ref); err != nil {
			t.Fatalf("target stat %s: %v", ref.URI(), err)
		}
	}

	staged, err := StageOut(context.Background(), spec, root, target, scope)
	if err != nil {
		t.Fatalf("StageOut: %v", err)
	}
	if staged.Outcome != domain.OutcomeSucceeded || staged.ResultDigest != result.ResultDigest {
		t.Fatalf("staged result = %+v", staged)
	}
	if staged.Outputs["build"] != "ok" {
		t.Fatalf("staged outputs = %+v", staged.Outputs)
	}
}

func TestStageInStagesPositionalInputs(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	first := putBlob(t, source, scope, "first")
	second := putBlob(t, source, scope, "second")
	spec.Inputs = []domain.CASRef{first, second}

	root := t.TempDir()
	manifest, err := StageIn(context.Background(), spec, source, root, scope)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	if manifest.Inputs["input-0"] != first || manifest.Inputs["input-1"] != second {
		t.Fatalf("manifest = %+v", manifest.Inputs)
	}
	if manifest.Inputs["input-0"] == manifest.Inputs["input-1"] {
		t.Fatal("distinct positional blobs collapsed")
	}
}

func TestStageInRefusesMismatchedExistingBlob(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	ref := putBlob(t, source, scope, "wanted-content")

	root := t.TempDir()
	key, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, ref.Hex())
	if err != nil {
		t.Fatalf("blob key: %v", err)
	}
	path := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir blob dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("forged-content"), 0o600); err != nil {
		t.Fatalf("write forged blob: %v", err)
	}

	spec.NamedInputs = map[string]domain.InputBinding{"snapshot": {Ref: ref, Kind: "tar.gz"}}
	_, err = StageIn(context.Background(), spec, source, root, scope)
	if !errors.Is(err, ErrIntegrityConflict) {
		t.Fatalf("StageIn error = %v, want ErrIntegrityConflict", err)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read forged blob: %v", readErr)
	}
	if string(raw) != "forged-content" {
		t.Fatal("StageIn must not overwrite mismatched local content")
	}
}

func TestStageOutRejectsCorruptedLocalBlob(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	ref := putBlob(t, source, scope, "original-content")
	spec.NamedInputs = map[string]domain.InputBinding{"snapshot": {Ref: ref, Kind: "tar.gz"}}

	root := t.TempDir()
	if _, err := StageIn(context.Background(), spec, source, root, scope); err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	if err := WriteResult(root, AttemptRefOf(spec), succeededResult(t), nil); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}

	local, err := localfs.New(root)
	if err != nil {
		t.Fatalf("open local: %v", err)
	}
	blobs, err := local.Blobs(context.Background())
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("blobs = %d, want 1", len(blobs))
	}
	if err := os.WriteFile(blobs[0].Path, []byte("tampered-content"), 0o600); err != nil {
		t.Fatalf("tamper blob: %v", err)
	}

	target := casmemory.New()
	result, err := StageOut(context.Background(), spec, root, target, scope)
	if err == nil {
		t.Fatal("expected StageOut to reject the corrupted blob")
	}
	if result.Outcome != "" {
		t.Fatalf("StageOut returned a result on failure: %+v", result)
	}
	class, _, retryable := Failure(err)
	if class != domain.ErrCASDigestMismatch || retryable {
		t.Fatalf("Failure = %s retryable=%v, want CAS_DIGEST_MISMATCH non-retryable", class, retryable)
	}
	if _, statErr := target.Stat(context.Background(), scope, ref); statErr == nil {
		t.Fatal("corrupted blob must not be uploaded")
	}
}

func TestStageOutMissingResult(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	ref := putBlob(t, source, scope, "artifact")
	spec.NamedInputs = map[string]domain.InputBinding{"snapshot": {Ref: ref, Kind: "tar.gz"}}

	root := t.TempDir()
	if _, err := StageIn(context.Background(), spec, source, root, scope); err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	_, err := StageOut(context.Background(), spec, root, casmemory.New(), scope)
	if !errors.Is(err, ErrResultMissing) {
		t.Fatalf("StageOut error = %v, want ErrResultMissing", err)
	}
	class, _, retryable := Failure(err)
	if class != domain.ErrValidationFailed || retryable {
		t.Fatalf("Failure = %s retryable=%v, want VALIDATION_FAILED non-retryable", class, retryable)
	}
}

func TestStageOutPartialUploadDoesNotSucceed(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	first := putBlob(t, source, scope, "first-blob")
	second := putBlob(t, source, scope, "second-blob")
	spec.NamedInputs = map[string]domain.InputBinding{
		"a": {Ref: first, Kind: "file"},
		"b": {Ref: second, Kind: "file"},
	}

	root := t.TempDir()
	if _, err := StageIn(context.Background(), spec, source, root, scope); err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	if err := WriteResult(root, AttemptRefOf(spec), succeededResult(t), nil); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}

	target := &failingStore{inner: casmemory.New(), failAfter: 1}
	result, err := StageOut(context.Background(), spec, root, target, scope)
	if err == nil {
		t.Fatal("expected a partial upload failure")
	}
	if result.Outcome != "" {
		t.Fatalf("StageOut must not return success after a partial upload: %+v", result)
	}
	if target.puts != 1 {
		t.Fatalf("target puts = %d, want 1 before failure", target.puts)
	}
	class, _, retryable := Failure(err)
	if class != domain.ErrUploadFailed || !retryable {
		t.Fatalf("Failure = %s retryable=%v, want UPLOAD_FAILED retryable", class, retryable)
	}
}

func TestStageOutReportsHardRunnerError(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	ref := putBlob(t, source, scope, "artifact")
	spec.NamedInputs = map[string]domain.InputBinding{"snapshot": {Ref: ref, Kind: "file"}}

	root := t.TempDir()
	if _, err := StageIn(context.Background(), spec, source, root, scope); err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	failed := domain.StepResult{
		Outcome:    domain.OutcomeFailed,
		ErrorClass: domain.ErrEngineCrash,
		Message:    "engine crashed",
		Retryable:  true,
		FinishedAt: time.Now().UTC(),
	}
	if err := WriteResult(root, AttemptRefOf(spec), failed, errors.New("runner setup failed")); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}

	result, err := StageOut(context.Background(), spec, root, casmemory.New(), scope)
	if err == nil {
		t.Fatal("expected a ResultError for a hard runner error")
	}
	if result.Outcome != domain.OutcomeFailed {
		t.Fatalf("result = %+v", result)
	}
	var resultErr *ResultError
	if !errors.As(err, &resultErr) {
		t.Fatalf("error = %T, want *ResultError", err)
	}
	if resultErr.Class != domain.ErrEngineCrash || !resultErr.Retryable {
		t.Fatalf("ResultError = %+v", resultErr)
	}
	if !strings.Contains(resultErr.Message, "runner setup failed") {
		t.Fatalf("message = %q", resultErr.Message)
	}
}

func TestStageOutRejectsAttemptMismatch(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	root := t.TempDir()
	other := testSpec()
	if err := WriteResult(root, AttemptRefOf(other), succeededResult(t), nil); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	_, err := StageOut(context.Background(), spec, root, casmemory.New(), scope)
	if !errors.Is(err, ErrAttemptMismatch) {
		t.Fatalf("StageOut error = %v, want ErrAttemptMismatch", err)
	}
}

func TestStageOutRejectsInvalidResult(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	root := t.TempDir()
	invalid := domain.StepResult{Outcome: domain.OutcomeSucceeded}
	if err := WriteResult(root, AttemptRefOf(spec), invalid, nil); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	_, err := StageOut(context.Background(), spec, root, casmemory.New(), scope)
	var resultErr *ResultError
	if !errors.As(err, &resultErr) || resultErr.Class != domain.ErrValidationFailed {
		t.Fatalf("StageOut error = %v, want validation ResultError", err)
	}
}

func TestReadResultRejectsMalformedDocument(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"garbage":   "{",
		"trailing":  `{"result":{"outcome":"SUCCEEDED","result_digest":"x"}} trailing`,
		"unknown":   `{"unexpected":true}`,
		"wrongtype": `{"attempt_ref":"not-an-object"}`,
	} {
		if err := os.WriteFile(filepath.Join(root, ResultFile), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := ReadResult(root); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("%s: ReadResult error = %v, want ErrInvalidDocument", name, err)
		}
	}
}

func TestReadResultRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte(`{"result":{}}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, ResultFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReadResult(root); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("ReadResult error = %v, want ErrInvalidDocument", err)
	}
}

func TestWriteResultIsOwnerOnly(t *testing.T) {
	root := t.TempDir()
	if err := WriteResult(root, AttemptRefOf(testSpec()), succeededResult(t), nil); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, ResultFile))
	if err != nil {
		t.Fatalf("stat result: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("result mode = %v, want owner-only", info.Mode())
	}
}

func TestStageInRejectsInvalidScopeAndSpec(t *testing.T) {
	source := casmemory.New()
	root := t.TempDir()
	spec := testSpec()
	scope := testScope(spec)
	scope.TenantID = ""
	if _, err := StageIn(context.Background(), spec, source, root, scope); err == nil {
		t.Fatal("expected invalid scope error")
	}
	if _, err := StageIn(context.Background(), spec, nil, root, testScope(spec)); err == nil {
		t.Fatal("expected missing remote store error")
	}
	if _, err := StageIn(context.Background(), spec, source, "", testScope(spec)); err == nil {
		t.Fatal("expected empty root error")
	}
	bad := spec
	bad.AttemptID = ""
	if _, err := StageIn(context.Background(), bad, source, root, testScope(spec)); err == nil {
		t.Fatal("expected invalid spec error")
	}
}

type failingStore struct {
	inner     *casmemory.Store
	failAfter int
	puts      int
}

func (s *failingStore) Put(ctx context.Context, scope ports.SecurityScope, r io.Reader) (domain.CASRef, error) {
	if s.puts >= s.failAfter {
		return domain.CASRef{}, errors.New("injected upload failure")
	}
	s.puts++
	return s.inner.Put(ctx, scope, r)
}

func (s *failingStore) Open(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (io.ReadCloser, error) {
	return s.inner.Open(ctx, scope, ref)
}

func (s *failingStore) Stat(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (ports.BlobInfo, error) {
	return s.inner.Stat(ctx, scope, ref)
}

func (s *failingStore) Delete(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) error {
	return s.inner.Delete(ctx, scope, ref)
}

func (s *failingStore) Health(ctx context.Context) error { return s.inner.Health(ctx) }

func TestStageInRejectsRemoteDigestMismatch(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	ref := putBlob(t, source, scope, "real-content")
	spec.NamedInputs = map[string]domain.InputBinding{"snapshot": {Ref: ref, Kind: "tar.gz"}}

	root := t.TempDir()
	_, err := StageIn(context.Background(), spec, &wrongBytesStore{inner: source}, root, scope)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("StageIn error = %v, want ErrDigestMismatch", err)
	}
	local, openErr := localfs.New(root)
	if openErr != nil {
		t.Fatalf("open local: %v", openErr)
	}
	blobs, listErr := local.Blobs(context.Background())
	if listErr != nil {
		t.Fatalf("blobs: %v", listErr)
	}
	if len(blobs) != 0 {
		t.Fatalf("mismatched bytes must not linger in the local CAS: %+v", blobs)
	}
	class, _, retryable := Failure(err)
	if class != domain.ErrCASDigestMismatch || retryable {
		t.Fatalf("Failure = %s retryable=%v", class, retryable)
	}
}

type wrongBytesStore struct {
	inner *casmemory.Store
}

func (s *wrongBytesStore) Put(ctx context.Context, scope ports.SecurityScope, r io.Reader) (domain.CASRef, error) {
	return s.inner.Put(ctx, scope, r)
}

func (s *wrongBytesStore) Open(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("forged-content")), nil
}

func (s *wrongBytesStore) Stat(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (ports.BlobInfo, error) {
	return s.inner.Stat(ctx, scope, ref)
}

func (s *wrongBytesStore) Delete(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) error {
	return s.inner.Delete(ctx, scope, ref)
}

func (s *wrongBytesStore) Health(ctx context.Context) error { return s.inner.Health(ctx) }

func TestUploadBlobsIsDeterministic(t *testing.T) {
	spec := testSpec()
	scope := testScope(spec)
	source := casmemory.New()
	var refs []domain.CASRef
	for _, content := range []string{"c", "a", "b"} {
		refs = append(refs, putBlob(t, source, scope, content))
	}
	spec.Inputs = refs
	root := t.TempDir()
	if _, err := StageIn(context.Background(), spec, source, root, scope); err != nil {
		t.Fatalf("StageIn: %v", err)
	}
	target := casmemory.New()
	first, err := UploadBlobs(context.Background(), root, target, scope)
	if err != nil {
		t.Fatalf("UploadBlobs: %v", err)
	}
	second, err := UploadBlobs(context.Background(), root, target, scope)
	if err != nil {
		t.Fatalf("second UploadBlobs: %v", err)
	}
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("uploaded lengths = %d, %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("upload order differs at %d: %v vs %v", i, first, second)
		}
	}
}
