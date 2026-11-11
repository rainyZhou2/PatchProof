package goanalyzer

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/patchproof/patchproof/pkg/stepopts"
)

// analysisCacheDirName is the per-step scratch directory used for Go caches.
const analysisCacheDirName = ".pp-goanalyzer"

// prepareAnalyzerEnvironment builds a minimal, sanitized environment for
// go/packages. Network access is disabled unless the step provides go.proxy.
// The returned environment never inherits arbitrary caller variables.
func prepareAnalyzerEnvironment(ctx context.Context, workspace string, options map[string]string, limits *limitSet) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base := workspace
	if strings.TrimSpace(base) == "" {
		dir, err := os.MkdirTemp("", "pp-goanalyzer-")
		if err != nil {
			return nil, fmt.Errorf("create analyzer workspace: %w", err)
		}
		base = dir
	}
	cacheRoot := filepath.Join(base, analysisCacheDirName)
	for _, dir := range []string{
		filepath.Join(cacheRoot, "home"),
		filepath.Join(cacheRoot, "tmp"),
		filepath.Join(cacheRoot, "gocache"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create analyzer cache directory: %w", err)
		}
	}

	if err := validateAnalyzerFlags(stepopts.Get(options, stepopts.GoFlags)); err != nil {
		return nil, err
	}
	env := []string{
		"HOME=" + filepath.Join(cacheRoot, "home"),
		"TMPDIR=" + filepath.Join(cacheRoot, "tmp"),
		"GOCACHE=" + filepath.Join(cacheRoot, "gocache"),
		"GO111MODULE=on",
		"GOTOOLCHAIN=local",
		"GOFLAGS=" + analyzerGoFlags(options),
		"GOPROXY=" + analyzerProxy(options, limits),
		"GOSUMDB=off",
		"GOWORK=off",
		"CGO_ENABLED=0",
	}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	if goroot := os.Getenv("GOROOT"); goroot != "" && filepath.IsAbs(goroot) {
		env = append(env, "GOROOT="+goroot)
	}
	// Inherit module caches only when the operator configured them explicitly
	// and the values are absolute, so relative paths cannot redirect writes
	// into the analyzed snapshot.
	if gopath := os.Getenv("GOPATH"); filepath.IsAbs(gopath) {
		env = append(env, "GOPATH="+gopath)
	} else if gopath != "" {
		limits.add("ignored relative GOPATH from the engine environment")
	}
	if modcache := os.Getenv("GOMODCACHE"); filepath.IsAbs(modcache) {
		env = append(env, "GOMODCACHE="+modcache)
	} else if modcache != "" {
		limits.add("ignored relative GOMODCACHE from the engine environment")
	}
	return env, nil
}

// validateAnalyzerFlags rejects flags that could execute arbitrary tools or
// redirect the module loader at files outside the snapshot.
func validateAnalyzerFlags(raw string) error {
	for _, field := range strings.Fields(raw) {
		for _, banned := range []string{"-toolexec", "-exec", "-modfile", "-overlay"} {
			if field == banned || strings.HasPrefix(field, banned+"=") {
				return fmt.Errorf("go.flags must not set %s", banned)
			}
		}
	}
	return nil
}

// analyzerGoFlags merges the mandatory module mode with caller-provided flags.
func analyzerGoFlags(options map[string]string) string {
	flags := "-mod=mod"
	extra := strings.TrimSpace(stepopts.Get(options, stepopts.GoFlags))
	if extra != "" {
		flags += " " + sanitizeEnvValue(extra)
	}
	return flags
}

// analyzerProxy returns the module proxy setting for the step. The default is
// off, which guarantees the analyzer never reaches the network.
func analyzerProxy(options map[string]string, limits *limitSet) string {
	proxy := strings.TrimSpace(stepopts.Get(options, stepopts.Goproxy))
	if proxy == "" || proxy == "off" {
		return "off"
	}
	if !validProxyValue(proxy) {
		limits.add("ignored invalid go.proxy option; module proxy disabled")
		return "off"
	}
	return proxy
}

// sanitizeEnvValue removes characters that could confuse environment parsing.
func sanitizeEnvValue(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.TrimSpace(value)
}

// validProxyValue accepts off, a URL-like proxy, or the go directive values.
func validProxyValue(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return value == "direct" || strings.Contains(value, "://") || strings.Contains(value, ",")
}

// syntaxPackage groups parsed files belonging to one import path.
type syntaxPackage struct {
	pkgPath string
	files   []*ast.File
}

// loadTypeInfo loads the snapshot with full type information. A non-nil error
// means the caller should fall back to syntax-only analysis; partial package
// errors are recorded as limitations but still produce usable type data.
func loadTypeInfo(ctx context.Context, root string, env []string, limits *limitSet) ([]*packages.Package, *token.FileSet, error) {
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return nil, nil, fmt.Errorf("snapshot has no go.mod: %w", err)
	}
	fset := token.NewFileSet()
	config := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedImports |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedTypesSizes |
			packages.NeedDeps |
			packages.NeedModule,
		Context: ctx,
		Dir:     root,
		Env:     env,
		Tests:   false,
		Fset:    fset,
	}
	loaded, err := packages.Load(config, "./...")
	if err != nil {
		return nil, fset, fmt.Errorf("load packages: %w", err)
	}
	limits.add("package loading is bounded by the step deadline; individual packages do not have separate timeouts")
	limits.add("go.work workspace mode is disabled (GOWORK=off); go.work files are ignored")
	usable := make([]*packages.Package, 0, len(loaded))
	for _, pkg := range loaded {
		if pkg == nil {
			continue
		}
		if len(pkg.Errors) > 0 {
			limits.add(fmt.Sprintf("package %s reported %d type/load errors", pkg.PkgPath, len(pkg.Errors)))
		}
		for index, pkgErr := range pkg.Errors {
			if index >= 5 {
				limits.add(fmt.Sprintf("additional errors for package %s suppressed", pkg.PkgPath))
				break
			}
			limits.add(fmt.Sprintf("package %s: %s", pkg.PkgPath, truncateMessage(pkgErr.Error(), 240)))
		}
		if pkg.Types == nil || pkg.TypesInfo == nil {
			limits.add(fmt.Sprintf("package %s loaded without complete type information", pkg.PkgPath))
			continue
		}
		usable = append(usable, pkg)
	}
	if len(usable) == 0 {
		return nil, fset, fmt.Errorf("no packages with type information were loaded")
	}
	return usable, fset, nil
}

// parseSnapshotSyntax parses every non-vendored, non-test Go file under root.
// It is the deterministic fallback when module loading is impossible.
func parseSnapshotSyntax(ctx context.Context, root, modulePath string, limits *limitSet) ([]syntaxPackage, *token.FileSet, error) {
	fset := token.NewFileSet()
	byPath := make(map[string]*syntaxPackage)
	var order []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			limits.add(fmt.Sprintf("syntax fallback skipped %s: %v", relativePath(root, path), walkErr))
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
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
			if fallbackSkippedDirNames[entry.Name()] || (strings.HasPrefix(entry.Name(), ".") && entry.Name() != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		relative := relativePath(root, path)
		if !isGoSource(relative) || isVendoredPath(relative) {
			return nil
		}
		raw, bounded, err := readFileBounded(path)
		if err != nil {
			limits.add(fmt.Sprintf("syntax fallback could not read %s: %v", relative, err))
			return nil
		}
		if bounded {
			limits.add(fmt.Sprintf("syntax fallback truncated %s at %d bytes", relative, maxFileBytes))
		}
		file, parseErr := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if file == nil {
			limits.add(fmt.Sprintf("syntax fallback could not parse %s: %v", relative, parseErr))
			return nil
		}
		if parseErr != nil {
			limits.add(fmt.Sprintf("syntax fallback parsed %s with errors: %v", relative, truncateMessage(parseErr.Error(), 240)))
		}
		pkgPath := packagePathFor(root, relative, modulePath)
		syntaxPkg, ok := byPath[pkgPath]
		if !ok {
			syntaxPkg = &syntaxPackage{pkgPath: pkgPath}
			byPath[pkgPath] = syntaxPkg
			order = append(order, pkgPath)
		}
		syntaxPkg.files = append(syntaxPkg.files, file)
		return nil
	})
	if err != nil {
		return nil, fset, err
	}
	sort.Strings(order)
	packagesOut := make([]syntaxPackage, 0, len(order))
	for _, pkgPath := range order {
		syntaxPkg := byPath[pkgPath]
		sort.Slice(syntaxPkg.files, func(i, j int) bool {
			return fset.Position(syntaxPkg.files[i].Pos()).Filename < fset.Position(syntaxPkg.files[j].Pos()).Filename
		})
		packagesOut = append(packagesOut, *syntaxPkg)
	}
	return packagesOut, fset, nil
}

// truncateMessage bounds a free-form message included in artifacts.
func truncateMessage(message string, limit int) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) <= limit {
		return message
	}
	if limit <= 3 {
		return message[:limit]
	}
	return message[:limit-3] + "..."
}
