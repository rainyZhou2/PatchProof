package goanalyzer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skippedDirNames are never traversed by snapshot scanning or the AST fallback.
var skippedDirNames = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
}

// fallbackSkippedDirNames additionally excludes directories that are not part
// of the compiled module surface but are safe to ignore for symbol discovery.
var fallbackSkippedDirNames = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
}

// fileEntry is the snapshot-relative metadata for one regular file.
type fileEntry struct {
	path   string
	size   int64
	mode   string
	digest string
	binary bool
}

// readFileBounded reads at most maxFileBytes of a file. Files larger than the
// cap are reported as bounded so callers can record a limitation.
func readFileBounded(path string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", path)
	}
	limited := info.Size() > maxFileBytes
	reader := io.Reader(file)
	if limited {
		reader = io.LimitReader(file, maxFileBytes)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, limited, err
	}
	return raw, limited, nil
}

// digestFile streams the full file contents into a SHA-256 digest. Memory use
// stays bounded regardless of the file size.
func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// detectBinary reports whether the first bytes of a file look binary. The
// heuristic mirrors git: a NUL byte in the leading window classifies the file
// as binary.
func detectBinary(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var prefix [8192]byte
	n, err := file.Read(prefix[:])
	if err != nil && err != io.EOF {
		return false
	}
	return bytes.IndexByte(prefix[:n], 0) >= 0
}

// scanSnapshot walks a snapshot root and records metadata for every regular
// file. Symbolic links are skipped so a checked-out tree cannot escape its
// root during analysis.
func scanSnapshot(ctx context.Context, root string, limits *limitSet) (map[string]fileEntry, bool, error) {
	files := make(map[string]fileEntry)
	truncated := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			limits.add(fmt.Sprintf("snapshot scan skipped %s: %v", path, walkErr))
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			if skippedDirNames[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			limits.add(fmt.Sprintf("snapshot scan skipped symbolic link %s", relativePath(root, path)))
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if len(files) >= maxSnapshotFiles {
			truncated = true
			limits.add(fmt.Sprintf("snapshot file count capped at %d; remaining files ignored", maxSnapshotFiles))
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			limits.add(fmt.Sprintf("snapshot scan could not stat %s: %v", relativePath(root, path), err))
			return nil
		}
		digest, err := digestFile(path)
		if err != nil {
			limits.add(fmt.Sprintf("snapshot scan could not hash %s: %v", relativePath(root, path), err))
			return nil
		}
		relative := relativePath(root, path)
		files[relative] = fileEntry{
			path:   relative,
			size:   info.Size(),
			mode:   fmt.Sprintf("%04o", info.Mode().Perm()),
			digest: digest,
			binary: detectBinary(path),
		}
		return nil
	})
	if err != nil {
		return nil, truncated, err
	}
	return files, truncated, nil
}

// relativePath returns a slash-separated snapshot-relative path.
func relativePath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

// buildFileDiff compares two snapshots and produces a sorted file-level diff.
// Content-identical add/delete pairs become RENAMED entries.
func buildFileDiff(base, head map[string]fileEntry, baseScanned, headScanned bool) ([]FileDiff, []string, bool) {
	limits := newLimitSet()
	truncated := baseScanned || headScanned
	if baseScanned {
		limits.add("base snapshot scan was truncated at the file cap")
	}
	if headScanned {
		limits.add("head snapshot scan was truncated at the file cap")
	}

	paths := make(map[string]struct{}, len(base)+len(head))
	for path := range base {
		paths[path] = struct{}{}
	}
	for path := range head {
		paths[path] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)

	var diffs []FileDiff
	var added []fileEntry
	var deleted []fileEntry
	for _, path := range ordered {
		baseEntry, hasBase := base[path]
		headEntry, hasHead := head[path]
		switch {
		case hasBase && hasHead:
			if baseEntry.digest == headEntry.digest && baseEntry.mode == headEntry.mode {
				continue
			}
			diffs = append(diffs, FileDiff{
				Path:       path,
				Change:     "MODIFIED",
				BaseDigest: baseEntry.digest,
				HeadDigest: headEntry.digest,
				BaseSize:   baseEntry.size,
				HeadSize:   headEntry.size,
				BaseMode:   baseEntry.mode,
				HeadMode:   headEntry.mode,
				Binary:     baseEntry.binary || headEntry.binary,
			})
		case hasBase:
			deleted = append(deleted, baseEntry)
		case hasHead:
			added = append(added, headEntry)
		}
	}

	addedByDigest := make(map[string][]fileEntry)
	for _, entry := range added {
		addedByDigest[entry.digest] = append(addedByDigest[entry.digest], entry)
	}
	pairedAdded := make(map[string]bool, len(added))
	pairedDeleted := make(map[string]bool, len(deleted))
	for _, entry := range deleted {
		candidates := addedByDigest[entry.digest]
		var match *fileEntry
		for index := range candidates {
			if !pairedAdded[candidates[index].path] {
				match = &candidates[index]
				break
			}
		}
		if match == nil {
			continue
		}
		pairedDeleted[entry.path] = true
		pairedAdded[match.path] = true
		diffs = append(diffs, FileDiff{
			Path:       match.path,
			Change:     "RENAMED",
			OldPath:    entry.path,
			BaseDigest: entry.digest,
			HeadDigest: match.digest,
			BaseSize:   entry.size,
			HeadSize:   match.size,
			BaseMode:   entry.mode,
			HeadMode:   match.mode,
			Binary:     entry.binary || match.binary,
		})
	}
	for _, entry := range deleted {
		if pairedDeleted[entry.path] {
			continue
		}
		diffs = append(diffs, FileDiff{
			Path:       entry.path,
			Change:     "DELETED",
			BaseDigest: entry.digest,
			BaseSize:   entry.size,
			BaseMode:   entry.mode,
			Binary:     entry.binary,
		})
	}
	for _, entry := range added {
		if pairedAdded[entry.path] {
			continue
		}
		diffs = append(diffs, FileDiff{
			Path:       entry.path,
			Change:     "ADDED",
			HeadDigest: entry.digest,
			HeadSize:   entry.size,
			HeadMode:   entry.mode,
			Binary:     entry.binary,
		})
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	if len(diffs) > maxChanges {
		limits.add(fmt.Sprintf("file diff capped at %d entries", maxChanges))
		diffs = diffs[:maxChanges]
		truncated = true
	}
	return diffs, limits.list(), truncated
}

// isGoSource reports whether a snapshot-relative path is a non-test Go source
// file eligible for AST analysis.
func isGoSource(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

// isTestSource reports whether a snapshot-relative path is a Go test file.
func isTestSource(path string) bool {
	return strings.HasSuffix(path, "_test.go")
}

// isVendoredPath reports whether a snapshot-relative path is excluded from
// syntax-only analysis.
func isVendoredPath(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if fallbackSkippedDirNames[segment] {
			return true
		}
	}
	return false
}

// moduleDirective returns the module path declared by a go.mod file, or an
// empty string when the file is absent or malformed. The parser intentionally
// handles only the single directive needed for symbol IDs.
func moduleDirective(root string) string {
	raw, _, err := readFileBounded(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "module") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "module"))
		if rest == trimmed {
			continue
		}
		if idx := strings.Index(rest, "//"); idx >= 0 {
			rest = strings.TrimSpace(rest[:idx])
		}
		rest = strings.Trim(rest, "\"`")
		if rest != "" {
			return rest
		}
	}
	return ""
}

// packagePathFor derives a Go import path for a snapshot-relative file using
// the snapshot's module directive.
func packagePathFor(root, relativeFile, modulePath string) string {
	dir := filepath.ToSlash(filepath.Dir(relativeFile))
	if dir == "." || dir == "" {
		if modulePath != "" {
			return modulePath
		}
		return "local"
	}
	if modulePath != "" {
		return modulePath + "/" + dir
	}
	return "local/" + dir
}
