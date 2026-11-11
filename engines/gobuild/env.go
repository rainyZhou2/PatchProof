package gobuild

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patchproof/patchproof/internal/engine"
)

const buildCacheDirName = ".pp-gobuild"

// buildWorkspace is the per-step scratch layout for toolchain caches.
type buildWorkspace struct {
	root       string
	cacheRoot  string
	home       string
	tmp        string
	gocache    string
	gopath     string
	gomodcache string
}

// prepareBuildWorkspace creates the per-step workspace and cache directories.
// The caller-provided workspace is preferred; otherwise a private temporary
// directory is created.
func prepareBuildWorkspace(request engine.Request) (buildWorkspace, error) {
	root := strings.TrimSpace(request.Workspace)
	if root == "" {
		created, err := os.MkdirTemp("", "pp-gobuild-")
		if err != nil {
			return buildWorkspace{}, fmt.Errorf("create build workspace: %w", err)
		}
		root = created
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return buildWorkspace{}, fmt.Errorf("resolve build workspace: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return buildWorkspace{}, fmt.Errorf("create build workspace directory: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return buildWorkspace{}, fmt.Errorf("resolve build workspace symlinks: %w", err)
	}
	workspace := buildWorkspace{root: resolvedRoot}
	workspace.cacheRoot = filepath.Join(resolvedRoot, buildCacheDirName)
	workspace.home = filepath.Join(workspace.cacheRoot, "home")
	workspace.tmp = filepath.Join(workspace.cacheRoot, "tmp")
	workspace.gocache = filepath.Join(workspace.cacheRoot, "gocache")
	workspace.gopath = filepath.Join(workspace.cacheRoot, "gopath")
	workspace.gomodcache = filepath.Join(workspace.cacheRoot, "gomodcache")
	for _, dir := range []string{
		workspace.cacheRoot,
		workspace.home,
		workspace.tmp,
		workspace.gocache,
		workspace.gopath,
		workspace.gomodcache,
		filepath.Join(workspace.cacheRoot, "logs"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return buildWorkspace{}, fmt.Errorf("create build cache directory: %w", err)
		}
	}
	resolvedCache, err := filepath.EvalSymlinks(workspace.cacheRoot)
	if err != nil {
		return buildWorkspace{}, fmt.Errorf("resolve build cache symlinks: %w", err)
	}
	if !isWithin(workspace.root, resolvedCache) {
		return buildWorkspace{}, fmt.Errorf("build cache directory escapes the step workspace")
	}
	return workspace, nil
}

// isWithin reports whether path is root or a descendant of root.
func isWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// stagedSnapshot resolves and validates a staged snapshot directory.
func stagedSnapshot(request engine.Request, name string) (string, error) {
	input, ok := request.Staged[name]
	if !ok || strings.TrimSpace(input.Path) == "" {
		return "", fmt.Errorf("staged %s snapshot is required", name)
	}
	absolute, err := filepath.Abs(input.Path)
	if err != nil {
		return "", fmt.Errorf("resolve staged %s snapshot: %w", name, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("staged %s snapshot is unavailable: %w", name, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("staged %s snapshot is unavailable: %w", name, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("staged %s snapshot is not a directory", name)
	}
	if filepath.Dir(resolved) == resolved {
		return "", fmt.Errorf("staged %s snapshot resolves to the filesystem root", name)
	}
	return resolved, nil
}

// buildEnvironment constructs the sanitized environment for toolchain commands.
// Only PATH, HOME, temporary storage and module/toolchain settings are exposed.
func buildEnvironment(workspace buildWorkspace, view optionsView) []string {
	gocache := workspace.gocache
	if external := os.Getenv("GOCACHE"); usableExternalDir(external) {
		gocache = external
	}
	gopath := workspace.gopath
	if external := os.Getenv("GOPATH"); usableExternalDir(external) {
		gopath = external
	}
	gomodcache := workspace.gomodcache
	if external := os.Getenv("GOMODCACHE"); usableExternalDir(external) {
		gomodcache = external
	}
	env := []string{
		"HOME=" + workspace.home,
		"TMPDIR=" + workspace.tmp,
		"GOCACHE=" + gocache,
		"GOPATH=" + gopath,
		"GOMODCACHE=" + gomodcache,
		"GO111MODULE=on",
		"GOTOOLCHAIN=local",
		"GOPROXY=" + view.proxy,
		"GOSUMDB=off",
		"GOWORK=off",
	}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	if goroot := os.Getenv("GOROOT"); goroot != "" && filepath.IsAbs(goroot) {
		env = append(env, "GOROOT="+goroot)
	}
	if view.goFlags != "" {
		env = append(env, "GOFLAGS="+view.goFlags)
	}
	if view.cgo {
		env = append(env, "CGO_ENABLED=1")
	} else {
		env = append(env, "CGO_ENABLED=0")
	}
	return env
}

// usableExternalDir reports whether an inherited directory is absolute and
// writable by the engine. Unusable values are replaced with workspace-local
// directories so toolchain writes never hit read-only host paths.
func usableExternalDir(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	probe, err := os.CreateTemp(path, ".pp-write-probe-*")
	if err != nil {
		return false
	}
	probeName := probe.Name()
	probe.Close()
	_ = os.Remove(probeName)
	return true
}
