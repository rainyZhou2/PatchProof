// Package broker implements the trusted halves of brokered offline artifact
// IO for PatchProof attempts.
//
// The flow has three phases:
//
//  1. stage-in (trusted, network allowed): copy every input blob from the
//     remote CAS into an attempt-scoped local CAS on a shared volume and record
//     an input manifest. Nothing is extracted; extraction is the worker's job.
//  2. execute (untrusted, no network, no credentials): the worker treats the
//     local CAS root as its ArtifactStore and writes result.json next to it.
//  3. stage-out (trusted, network allowed): verify and upload every local blob,
//     then read result.json and hand the result to the controller callback.
//
// This package only performs phase 1 and phase 3. It never extracts archives,
// never runs subject code, and never talks to the controller.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patchproof/patchproof/internal/adapters/cas/localfs"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	// InputManifestFile is the name of the stage-in manifest inside the local
	// artifact root.
	InputManifestFile = "input-manifest.json"
	// ResultFile is the name of the worker result document inside the local
	// artifact root.
	ResultFile = "result.json"
	// MaxDocumentBytes bounds manifest and result documents.
	MaxDocumentBytes = 64 << 20
	// MaxFailureMessage bounds the message embedded in a failure report.
	MaxFailureMessage = 8192

	maxPositionalInputs = 1 << 16
	documentPerm        = 0o600
	directoryPerm       = 0o750
)

var (
	// ErrDigestMismatch reports staged bytes that do not match the declared
	// content address.
	ErrDigestMismatch = errors.New("broker: digest mismatch")
	// ErrIntegrityConflict reports pre-existing local content that does not
	// match the digest it would be staged under.
	ErrIntegrityConflict = errors.New("broker: local blob conflicts with the declared digest")
	// ErrResultMissing reports a missing worker result document.
	ErrResultMissing = errors.New("broker: worker result document is missing")
	// ErrAttemptMismatch reports a result document produced by a different
	// attempt than the one being staged out.
	ErrAttemptMismatch = errors.New("broker: result document belongs to a different attempt")
	// ErrInvalidDocument reports a malformed manifest or result document.
	ErrInvalidDocument = errors.New("broker: invalid document")
)

// InputManifest is written to InputManifestFile after a successful stage-in.
// It maps each named input (and each positional input as "input-<index>") to
// the content address staged into the local CAS.
type InputManifest struct {
	AttemptRef domain.AttemptRef        `json:"attempt_ref"`
	Inputs     map[string]domain.CASRef `json:"inputs"`
}

// ResultDocument is the stable document written by the offline worker and read
// by stage-out:
//
//	{
//	  "attempt_ref": {"tenant_id": "...", "run_id": "...", "step_id": "...",
//	                   "attempt_id": "...", "generation": 1},
//	  "result": { ...domain.StepResult... },
//	  "error": "optional hard runner error"
//	}
//
// When error is empty the broker reports Complete with result. When error is
// non-empty the broker reports Fail (the result may still carry the engine's
// failure classification).
type ResultDocument struct {
	AttemptRef domain.AttemptRef `json:"attempt_ref"`
	Result     domain.StepResult `json:"result"`
	Error      string            `json:"error,omitempty"`
}

// ResultError classifies a result document that cannot be reported as a normal
// completion.
type ResultError struct {
	Class     domain.ErrorClass
	Message   string
	Retryable bool
	Cause     error
}

func (e *ResultError) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = "result document rejected"
	}
	return "broker: " + message
}

// Unwrap exposes the underlying sentinel so callers can use errors.Is.
func (e *ResultError) Unwrap() error { return e.Cause }

// AttemptRefOf derives the attempt identity carried by a step spec.
func AttemptRefOf(spec domain.StepSpec) domain.AttemptRef {
	return domain.AttemptRef{
		TenantID: spec.TenantID, RunID: spec.RunID, StepID: spec.StepID,
		AttemptID: spec.AttemptID, Generation: spec.Generation,
	}
}

// StageIn copies every named and positional input from remote into a local CAS
// rooted at localRoot. Each blob is hash-verified against its CAS ref before it
// becomes visible. The returned manifest is written to
// <localRoot>/input-manifest.json.
func StageIn(ctx context.Context, spec domain.StepSpec, remote ports.ArtifactStore, localRoot string, scope ports.SecurityScope) (InputManifest, error) {
	manifest := InputManifest{AttemptRef: AttemptRefOf(spec), Inputs: map[string]domain.CASRef{}}
	if remote == nil {
		return manifest, errors.New("broker stage-in: remote artifact store is required")
	}
	if err := validateSpecIdentity(spec); err != nil {
		return manifest, fmt.Errorf("broker stage-in: %w", err)
	}
	if err := scope.Validate(); err != nil {
		return manifest, fmt.Errorf("broker stage-in: %w", err)
	}
	root, err := ensureRoot(localRoot)
	if err != nil {
		return manifest, fmt.Errorf("broker stage-in: %w", err)
	}
	local, err := localfs.New(root)
	if err != nil {
		return manifest, fmt.Errorf("broker stage-in: %w", err)
	}

	names := make([]string, 0, len(spec.NamedInputs))
	for name := range spec.NamedInputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return manifest, fmt.Errorf("broker stage-in: input name must not be empty")
		}
		binding := spec.NamedInputs[name]
		if err := binding.Validate(); err != nil {
			return manifest, fmt.Errorf("broker stage-in: input %s: %w", name, err)
		}
		if err := stageRef(ctx, remote, local, scope, binding.Ref); err != nil {
			return manifest, fmt.Errorf("broker stage-in: input %s: %w", name, err)
		}
		manifest.Inputs[name] = binding.Ref
	}
	if len(spec.Inputs) > maxPositionalInputs {
		return manifest, fmt.Errorf("broker stage-in: %d positional inputs exceeds limit %d", len(spec.Inputs), maxPositionalInputs)
	}
	for index, ref := range spec.Inputs {
		name := fmt.Sprintf("input-%d", index)
		if _, duplicate := manifest.Inputs[name]; duplicate {
			return manifest, fmt.Errorf("broker stage-in: positional input name %q collides with a named input", name)
		}
		if err := ref.Validate(); err != nil {
			return manifest, fmt.Errorf("broker stage-in: positional input %d: %w", index, err)
		}
		if err := stageRef(ctx, remote, local, scope, ref); err != nil {
			return manifest, fmt.Errorf("broker stage-in: positional input %d: %w", index, err)
		}
		manifest.Inputs[name] = ref
	}
	if err := writeJSONAtomic(root, InputManifestFile, manifest); err != nil {
		return manifest, fmt.Errorf("broker stage-in: write manifest: %w", err)
	}
	return manifest, nil
}

// stageRef materialises one ref in the local CAS. An existing blob is verified
// (not overwritten); a newly streamed blob is accepted only when the digest
// computed from the bytes matches the ref. A mismatch deletes the just-written
// blob so a bad byte stream cannot linger in the local CAS.
func stageRef(ctx context.Context, remote ports.ArtifactStore, local *localfs.Store, scope ports.SecurityScope, ref domain.CASRef) error {
	if _, err := local.Stat(ctx, scope, ref); err == nil {
		if err := verifyLocalBlob(ctx, local, scope, ref); err != nil {
			return fmt.Errorf("%w: existing local blob %s is not valid: %w", ErrIntegrityConflict, ref.URI(), err)
		}
		return nil
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fmt.Errorf("stat local blob %s: %w", ref.URI(), err)
	}

	reader, err := remote.Open(ctx, scope, ref)
	if err != nil {
		return fmt.Errorf("open remote blob %s: %w", ref.URI(), err)
	}
	written, putErr := local.Put(ctx, scope, reader)
	closeErr := reader.Close()
	if putErr != nil {
		return fmt.Errorf("stage remote blob %s: %w", ref.URI(), putErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close remote blob %s: %w", ref.URI(), closeErr)
	}
	if written.Hex() != ref.Hex() || written.SizeBytes != ref.SizeBytes {
		_ = local.Delete(ctx, scope, written)
		return fmt.Errorf("%w: staged %s (%d bytes), expected %s (%d bytes)",
			ErrDigestMismatch, written.URI(), written.SizeBytes, ref.URI(), ref.SizeBytes)
	}
	return nil
}

func verifyLocalBlob(ctx context.Context, local *localfs.Store, scope ports.SecurityScope, ref domain.CASRef) error {
	reader, err := local.Open(ctx, scope, ref)
	if err != nil {
		return err
	}
	_, readErr := io.Copy(io.Discard, reader)
	closeErr := reader.Close()
	return errors.Join(readErr, closeErr)
}

// UploadBlobs walks the local CAS, verifies every blob against its path digest
// and uploads it to remote. It returns the uploaded refs in digest order. Any
// verification or upload failure aborts the walk; the caller must not report
// success for a partial upload.
func UploadBlobs(ctx context.Context, localRoot string, remote ports.ArtifactStore, scope ports.SecurityScope) ([]domain.CASRef, error) {
	if remote == nil {
		return nil, errors.New("broker upload: remote artifact store is required")
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("broker upload: %w", err)
	}
	local, err := localfs.New(localRoot)
	if err != nil {
		return nil, fmt.Errorf("broker upload: %w", err)
	}
	blobs, err := local.Blobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("broker upload: %w", err)
	}
	uploaded := make([]domain.CASRef, 0, len(blobs))
	for _, blob := range blobs {
		ref, err := uploadBlob(ctx, local, remote, scope, blob.Ref)
		if err != nil {
			return uploaded, err
		}
		uploaded = append(uploaded, ref)
	}
	return uploaded, nil
}

func uploadBlob(ctx context.Context, local *localfs.Store, remote ports.ArtifactStore, scope ports.SecurityScope, ref domain.CASRef) (domain.CASRef, error) {
	reader, err := local.Open(ctx, scope, ref)
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("broker upload: verify local blob %s: %w", ref.URI(), err)
	}
	stored, putErr := remote.Put(ctx, scope, reader)
	closeErr := reader.Close()
	if putErr != nil {
		return domain.CASRef{}, fmt.Errorf("broker upload: upload %s: %w", ref.URI(), putErr)
	}
	if closeErr != nil {
		return domain.CASRef{}, fmt.Errorf("broker upload: verify local blob %s: %w", ref.URI(), closeErr)
	}
	if stored.Hex() != ref.Hex() || stored.SizeBytes != ref.SizeBytes {
		return domain.CASRef{}, fmt.Errorf("%w: remote stored %s (%d bytes) for local %s (%d bytes)",
			ErrDigestMismatch, stored.URI(), stored.SizeBytes, ref.URI(), ref.SizeBytes)
	}
	return stored, nil
}

// StageOut uploads every local blob, then reads and validates result.json and
// returns the worker result. A failed StepResult is a valid result (the
// controller records the failure); a hard runner error or malformed document is
// returned as *ResultError so the caller reports Fail instead of Complete.
func StageOut(ctx context.Context, spec domain.StepSpec, localRoot string, remote ports.ArtifactStore, scope ports.SecurityScope) (domain.StepResult, error) {
	if _, err := UploadBlobs(ctx, localRoot, remote, scope); err != nil {
		return domain.StepResult{}, err
	}
	document, err := ReadResult(localRoot)
	if err != nil {
		return domain.StepResult{}, err
	}
	if document.AttemptRef != (domain.AttemptRef{}) {
		if err := document.AttemptRef.Validate(); err != nil {
			return document.Result, &ResultError{
				Class: domain.ErrValidationFailed, Message: "invalid attempt ref: " + err.Error(), Retryable: false,
			}
		}
		if document.AttemptRef.AttemptID != spec.AttemptID || document.AttemptRef.Generation != spec.Generation {
			return document.Result, &ResultError{
				Class: domain.ErrValidationFailed,
				Message: fmt.Sprintf("%s: result document is for attempt %s generation %d, expected %s generation %d",
					ErrAttemptMismatch, document.AttemptRef.AttemptID, document.AttemptRef.Generation, spec.AttemptID, spec.Generation),
				Retryable: false,
				Cause:     ErrAttemptMismatch,
			}
		}
	}
	if err := document.Result.Validate(); err != nil {
		class := domain.ErrValidationFailed
		message := "invalid step result: " + err.Error()
		if document.Error != "" {
			class = domain.ErrEngineCrash
			message = document.Error + " (invalid step result: " + err.Error() + ")"
		}
		return document.Result, &ResultError{Class: class, Message: message, Retryable: false}
	}
	if document.Error != "" {
		class := domain.ErrEngineCrash
		if document.Result.Outcome == domain.OutcomeFailed && document.Result.ErrorClass.Valid() {
			class = document.Result.ErrorClass
		}
		return document.Result, &ResultError{
			Class: class, Message: document.Error, Retryable: document.Result.Retryable || class.Retryable(),
		}
	}
	return document.Result, nil
}

// WriteResult writes the offline worker result document into localRoot using an
// atomic temp+rename. runErr is recorded as the document error field.
func WriteResult(localRoot string, attemptRef domain.AttemptRef, result domain.StepResult, runErr error) error {
	document := ResultDocument{AttemptRef: attemptRef, Result: result}
	if runErr != nil {
		document.Error = runErr.Error()
	}
	return WriteResultDocument(localRoot, document)
}

// WriteResultDocument writes an already assembled result document.
func WriteResultDocument(localRoot string, document ResultDocument) error {
	root, err := ensureRoot(localRoot)
	if err != nil {
		return err
	}
	if err := writeJSONAtomic(root, ResultFile, document); err != nil {
		return fmt.Errorf("broker: write result document: %w", err)
	}
	return nil
}

// ReadResult reads the result document from localRoot.
func ReadResult(localRoot string) (ResultDocument, error) {
	root, err := ensureRoot(localRoot)
	if err != nil {
		return ResultDocument{}, err
	}
	path := filepath.Join(root, ResultFile)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return ResultDocument{}, fmt.Errorf("%w: %s", ErrResultMissing, path)
		}
		return ResultDocument{}, fmt.Errorf("broker: stat result document: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ResultDocument{}, fmt.Errorf("%w: %s is not a regular file", ErrInvalidDocument, path)
	}
	if info.Size() > MaxDocumentBytes {
		return ResultDocument{}, fmt.Errorf("%w: result document exceeds %d bytes", ErrInvalidDocument, MaxDocumentBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return ResultDocument{}, fmt.Errorf("broker: open result document: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		return ResultDocument{}, fmt.Errorf("%w: read result document: %w", ErrInvalidDocument, err)
	}
	if int64(len(data)) > MaxDocumentBytes {
		return ResultDocument{}, fmt.Errorf("%w: result document exceeds %d bytes", ErrInvalidDocument, MaxDocumentBytes)
	}
	var document ResultDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return ResultDocument{}, fmt.Errorf("%w: decode result document: %w", ErrInvalidDocument, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ResultDocument{}, fmt.Errorf("%w: result document has trailing content", ErrInvalidDocument)
	}
	return document, nil
}

// Failure classifies an error returned by StageOut for a failure callback.
func Failure(err error) (domain.ErrorClass, string, bool) {
	if err == nil {
		return "", "", false
	}
	var resultErr *ResultError
	if errors.As(err, &resultErr) {
		class := resultErr.Class
		if !class.Valid() {
			class = domain.ErrExecutorError
		}
		return class, truncate(strings.TrimSpace(resultErr.Message), MaxFailureMessage), resultErr.Retryable
	}
	switch {
	case errors.Is(err, ErrResultMissing), errors.Is(err, ErrInvalidDocument), errors.Is(err, ErrAttemptMismatch):
		return domain.ErrValidationFailed, truncate(err.Error(), MaxFailureMessage), false
	case errors.Is(err, ErrDigestMismatch), errors.Is(err, ErrIntegrityConflict), errors.Is(err, localfs.ErrIntegrity):
		return domain.ErrCASDigestMismatch, truncate(err.Error(), MaxFailureMessage), false
	default:
		return domain.ErrUploadFailed, truncate(err.Error(), MaxFailureMessage), true
	}
}

func validateSpecIdentity(spec domain.StepSpec) error {
	if err := spec.TenantID.Validate(); err != nil {
		return err
	}
	if err := spec.RunID.Validate(); err != nil {
		return err
	}
	if err := spec.StepID.Validate(); err != nil {
		return err
	}
	if err := spec.AttemptID.Validate(); err != nil {
		return err
	}
	if spec.Generation <= 0 {
		return fmt.Errorf("generation must be positive")
	}
	return nil
}

func ensureRoot(localRoot string) (string, error) {
	trimmed := strings.TrimSpace(localRoot)
	if trimmed == "" {
		return "", errors.New("local artifact root must not be empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve local artifact root %q: %w", trimmed, err)
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, directoryPerm); err != nil {
		return "", fmt.Errorf("create local artifact root: %w", err)
	}
	return absolute, nil
}

func writeJSONAtomic(root, name string, payload any) error {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temp, err := os.CreateTemp(root, "."+name+"-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	discard := func() {
		temp.Close()
		os.Remove(tempName)
	}
	if err := temp.Chmod(documentPerm); err != nil {
		discard()
		return err
	}
	if _, err := temp.Write(encoded); err != nil {
		discard()
		return err
	}
	if err := temp.Sync(); err != nil {
		discard()
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempName)
		return err
	}
	if err := os.Rename(tempName, filepath.Join(root, name)); err != nil {
		os.Remove(tempName)
		return err
	}
	return nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
