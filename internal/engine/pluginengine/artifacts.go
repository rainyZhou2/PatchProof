package pluginengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/plugin"
)

const outputDirRoot = ".patchproof/plugin"

// uploadedFile keeps the CAS reference and host-validated media type for one
// plugin output file. engine.Result only carries CAS references, so the media
// type is only used for diagnostics.
type uploadedFile struct {
	Ref       domain.CASRef
	MediaType string
}

// prepareOutputDir creates a fresh 0700 directory under
// <workspace>/.patchproof/plugin/<step>-<attempt>. Stale contents from a losing
// attempt are removed before the plugin runs.
func prepareOutputDir(workspace string, stepID domain.StepID, attemptID domain.AttemptID) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", errors.New("workspace is required")
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(absWorkspace)
	if err != nil {
		return "", fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("workspace is not a directory")
	}
	base := filepath.Join(absWorkspace, filepath.FromSlash(outputDirRoot))
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create plugin output root: %w", err)
	}
	name := sanitize(stepID.String() + "-" + attemptID.String())
	dir := filepath.Join(base, name)
	relative, err := filepath.Rel(absWorkspace, dir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return "", errors.New("output directory escapes the workspace")
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("reset plugin output directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create plugin output directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("secure plugin output directory: %w", err)
	}
	return dir, nil
}

// resolveOutputPath converts a plugin-supplied relative path into a path inside
// root. Absolute paths, paths that escape root, symbolic links (including
// symlinked parent directories), and non-regular files are rejected.
func resolveOutputPath(root, relative string) (string, os.FileInfo, error) {
	if strings.TrimSpace(relative) == "" {
		return "", nil, errors.New("file path is required")
	}
	if strings.ContainsRune(relative, 0) {
		return "", nil, errors.New("file path contains NUL")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == string(os.PathSeparator) {
		return "", nil, fmt.Errorf("file path %q is not a file", relative)
	}
	if filepath.IsAbs(clean) {
		return "", nil, fmt.Errorf("file path %q must be relative to the output directory", relative)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", nil, fmt.Errorf("file path %q escapes the output directory", relative)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", nil, fmt.Errorf("resolve output directory: %w", err)
	}
	rootInfo, err := os.Lstat(absRoot)
	if err != nil {
		return "", nil, fmt.Errorf("stat output directory: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("output directory is a symbolic link")
	}
	if !rootInfo.IsDir() {
		return "", nil, errors.New("output directory is not a directory")
	}
	target := filepath.Join(absRoot, clean)
	inside, err := filepath.Rel(absRoot, target)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(os.PathSeparator)) || filepath.IsAbs(inside) {
		return "", nil, fmt.Errorf("file path %q escapes the output directory", relative)
	}

	current := absRoot
	segments := strings.Split(inside, string(os.PathSeparator))
	for index, segment := range segments {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return "", nil, fmt.Errorf("stat %q: %w", inside, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("file path %q traverses a symbolic link", inside)
		}
		if index == len(segments)-1 {
			if !info.Mode().IsRegular() {
				return "", nil, fmt.Errorf("file path %q is not a regular file", inside)
			}
			return current, info, nil
		}
		if !info.IsDir() {
			return "", nil, fmt.Errorf("file path %q traverses a non-directory", inside)
		}
	}
	return "", nil, fmt.Errorf("file path %q is not a file", relative)
}

// uploadFiles validates and uploads every plugin output file. It returns the
// name->ref map used by evidence and counterexample drafts.
func uploadFiles(ctx context.Context, store ports.ArtifactStore, scope ports.SecurityScope, outputDir string, files []plugin.FileArtifact, logger *slog.Logger) (map[string]uploadedFile, []domain.CASRef, error) {
	if len(files) > maxOutputFiles {
		return nil, nil, fmt.Errorf("plugin returned %d files, limit is %d", len(files), maxOutputFiles)
	}
	uploaded := make(map[string]uploadedFile, len(files))
	refs := make([]domain.CASRef, 0, len(files))
	var total int64
	for _, file := range files {
		name := strings.TrimSpace(file.Name)
		if name == "" {
			return nil, nil, errors.New("file artifact name is required")
		}
		if strings.ContainsRune(name, 0) {
			return nil, nil, fmt.Errorf("file artifact name %q contains NUL", name)
		}
		if _, duplicate := uploaded[name]; duplicate {
			return nil, nil, fmt.Errorf("duplicate file artifact name %q", name)
		}
		path, info, err := resolveOutputPath(outputDir, file.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("file artifact %q: %w", name, err)
		}
		if info.Size() < 0 || info.Size() > maxOutputBytes-total {
			return nil, nil, fmt.Errorf("plugin output files exceed the %d byte limit", maxOutputBytes)
		}
		mediaType, err := normalizeMediaType(file.MediaType)
		if err != nil {
			return nil, nil, fmt.Errorf("file artifact %q: %w", name, err)
		}
		total += info.Size()
		ref, err := engine.PutFile(ctx, store, scope, path)
		if err != nil {
			return nil, nil, fmt.Errorf("upload file artifact %q: %w", name, err)
		}
		if ref.SizeBytes != info.Size() {
			return nil, nil, fmt.Errorf("file artifact %q changed size during upload", name)
		}
		uploaded[name] = uploadedFile{Ref: ref, MediaType: mediaType}
		refs = append(refs, ref)
		if logger != nil {
			logger.Debug("plugin output file uploaded",
				slog.String("name", name),
				slog.String("media_type", mediaType),
				slog.String("cas_ref", ref.URI()),
			)
		}
	}
	return uploaded, refs, nil
}

func normalizeMediaType(raw string) (string, error) {
	mediaType := strings.TrimSpace(raw)
	if mediaType == "" {
		return "", nil
	}
	if len(mediaType) > 255 {
		return "", errors.New("media type is too long")
	}
	for _, char := range mediaType {
		if char < 0x20 || char == 0x7f {
			return "", errors.New("media type contains control characters")
		}
	}
	if !strings.Contains(mediaType, "/") {
		return "", fmt.Errorf("media type %q must be type/subtype", mediaType)
	}
	return mediaType, nil
}

// sanitize keeps the output directory name inside a known-safe alphabet. Step
// and attempt identifiers are UUIDs, so this is defence in depth.
func sanitize(value string) string {
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
	}
	out := builder.String()
	if len(out) > 96 {
		out = out[:96]
	}
	if out == "" {
		return "step"
	}
	return out
}
