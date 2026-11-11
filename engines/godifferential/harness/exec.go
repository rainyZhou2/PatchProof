package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Environment is the scrubbed process environment used for every toolchain and
// harness invocation. Only the variables required to build and run Go code
// inside the step workspace are propagated; the host environment is not
// inherited.
type Environment struct {
	GoBinary   string
	Workspace  string
	PATH       string
	HOME       string
	GOCache    string
	GOModCache string
	GOPATH     string
	Proxy      string
	TZ         string
}

// NewEnvironment prepares workspace-local Go caches. proxy is the configured
// module proxy; an empty value means "off" so that builds never reach the
// network unless the step explicitly allows a proxy.
func NewEnvironment(ctx context.Context, workspace, proxy string) (Environment, error) {
	_ = ctx
	if strings.TrimSpace(workspace) == "" {
		return Environment{}, fmt.Errorf("workspace is required")
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return Environment{}, fmt.Errorf("go toolchain is not available: %w", err)
	}
	root := filepath.Join(workspace, ".patchproof-toolchain")
	env := Environment{
		GoBinary:   goBinary,
		Workspace:  workspace,
		PATH:       os.Getenv("PATH"),
		HOME:       filepath.Join(root, "home"),
		GOCache:    filepath.Join(root, "gocache"),
		GOModCache: filepath.Join(root, "gomodcache"),
		GOPATH:     filepath.Join(root, "gopath"),
		Proxy:      strings.TrimSpace(proxy),
		TZ:         "UTC",
	}
	if env.PATH == "" {
		env.PATH = "/usr/local/bin:/usr/bin:/bin"
	}
	if env.HOME == "" {
		env.HOME = workspace
	}
	if env.Proxy == "" {
		env.Proxy = "off"
	}
	for _, dir := range []string{env.GOCache, env.GOModCache, env.GOPATH, filepath.Join(workspace, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Environment{}, fmt.Errorf("create workspace directory %s: %w", dir, err)
		}
	}
	return env, nil
}

// EnvVars returns the scrubbed child environment. GOTOOLCHAIN=local prevents the
// Go command from downloading a different toolchain, which would require
// network access.
func (e Environment) EnvVars() []string {
	return []string{
		"PATH=" + e.PATH,
		"HOME=" + e.HOME,
		"GOCACHE=" + e.GOCache,
		"GOMODCACHE=" + e.GOModCache,
		"GOPATH=" + e.GOPATH,
		"GOPROXY=" + e.Proxy,
		"CGO_ENABLED=0",
		"TZ=" + e.TZ,
		"GOTOOLCHAIN=local",
	}
}

// ProcessSpec describes one isolated child process.
type ProcessSpec struct {
	Dir       string
	Argv      []string
	Stdin     []byte
	Env       []string
	Timeout   time.Duration
	MaxStdout int
	MaxStderr int
}

// ProcessResult is the outcome of a child process invocation.
type ProcessResult struct {
	ExitCode       int
	Stdout         []byte
	Stderr         []byte
	TimedOut       bool
	Canceled       bool
	OutputOverflow bool
	Duration       time.Duration
	StartError     error
}

// Successful reports whether the process ran to completion with a zero exit
// code and bounded output.
func (r ProcessResult) Successful() bool {
	return r.StartError == nil && r.ExitCode == 0 && !r.TimedOut && !r.Canceled && !r.OutputOverflow
}

// Diagnostic renders a bounded, human-readable failure description.
func (r ProcessResult) Diagnostic() string {
	var parts []string
	switch {
	case r.StartError != nil:
		parts = append(parts, "start error: "+r.StartError.Error())
	case r.TimedOut:
		parts = append(parts, "timed out")
	case r.Canceled:
		parts = append(parts, "canceled")
	}
	if r.ExitCode != 0 {
		parts = append(parts, "exit code "+strconv.Itoa(r.ExitCode))
	}
	if r.OutputOverflow {
		parts = append(parts, "output exceeded capture limit")
	}
	if len(r.Stderr) > 0 {
		parts = append(parts, "stderr: "+TruncateForLog(r.Stderr, 2048))
	}
	if len(parts) == 0 {
		return "ok"
	}
	return strings.Join(parts, "; ")
}

type cappedWriter struct {
	limit    int
	buffer   bytes.Buffer
	overflow bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buffer.Len()
	if remaining <= 0 {
		w.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		w.buffer.Write(p[:remaining])
		w.overflow = true
		return len(p), nil
	}
	w.buffer.Write(p)
	return len(p), nil
}

// Run executes one child process with its own process group, a bounded output
// capture area and a hard timeout. Commands are always passed as argument
// vectors; no shell is involved.
func Run(ctx context.Context, spec ProcessSpec) ProcessResult {
	result := ProcessResult{ExitCode: -1}
	if len(spec.Argv) == 0 {
		result.StartError = fmt.Errorf("empty command")
		return result
	}
	if spec.MaxStdout <= 0 {
		spec.MaxStdout = 16 << 20
	}
	if spec.MaxStderr <= 0 {
		spec.MaxStderr = 1 << 20
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	stdout := &cappedWriter{limit: spec.MaxStdout}
	stderr := &cappedWriter{limit: spec.MaxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	configureProcess(cmd)

	started := time.Now()
	if err := cmd.Start(); err != nil {
		result.StartError = err
		result.Duration = time.Since(started)
		return result
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-waitCtx.Done():
		result.TimedOut = waitCtx.Err() == context.DeadlineExceeded
		result.Canceled = !result.TimedOut
		killProcessGroup(cmd.Process)
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			waitErr = <-done
		}
	}
	result.Duration = time.Since(started)
	result.Stdout = append([]byte(nil), stdout.buffer.Bytes()...)
	result.Stderr = append([]byte(nil), stderr.buffer.Bytes()...)
	result.OutputOverflow = stdout.overflow || stderr.overflow
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else if result.StartError == nil {
			result.StartError = waitErr
			result.ExitCode = -1
		}
	} else {
		result.ExitCode = 0
	}
	return result
}

// WriteSource installs the generated harness at
// <snapshot>/.patchproof/harness/main.go. The returned cleanup removes only the
// generated directory (and .patchproof when this run created it).
func WriteSource(snapshot string, source []byte) (func(), error) {
	root := filepath.Join(snapshot, ".patchproof")
	createdRoot := false
	if _, err := os.Stat(root); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", root, err)
		}
		createdRoot = true
	}
	dir := filepath.Join(root, "harness")
	createdDir := false
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", dir, err)
		}
		createdDir = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create harness directory: %w", err)
	}
	path := filepath.Join(dir, "main.go")
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("refusing to overwrite existing %s", path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, source, 0o644); err != nil {
		if createdDir {
			_ = os.RemoveAll(dir)
		}
		return nil, fmt.Errorf("write harness source: %w", err)
	}
	cleanup := func() {
		if createdDir {
			_ = os.RemoveAll(dir)
		} else {
			_ = os.Remove(path)
		}
		if createdRoot {
			_ = os.Remove(root)
		}
	}
	return cleanup, nil
}

// Build compiles one package relative to snapshotDir into output.
func (e Environment) Build(ctx context.Context, snapshotDir, output, packagePath string, timeout time.Duration) ProcessResult {
	return Run(ctx, ProcessSpec{
		Dir:       snapshotDir,
		Argv:      []string{e.GoBinary, "build", "-o", output, packagePath},
		Env:       e.EnvVars(),
		Timeout:   timeout,
		MaxStdout: 4 << 20,
		MaxStderr: 8 << 20,
	})
}

// RunHarness executes a built harness binary in single or batch mode.
func (e Environment) RunHarness(ctx context.Context, binary, dir string, stdin []byte, batch bool, timeout time.Duration, maxStdout int) ProcessResult {
	mode := "-single"
	if batch {
		mode = "-batch"
	}
	return Run(ctx, ProcessSpec{
		Dir:       dir,
		Argv:      []string{binary, mode},
		Stdin:     stdin,
		Env:       e.EnvVars(),
		Timeout:   timeout,
		MaxStdout: maxStdout,
		MaxStderr: 1 << 20,
	})
}

// Describe runs the harness in -describe mode and decodes the signature.
func (e Environment) Describe(ctx context.Context, binary, dir string, timeout time.Duration) (Signature, ProcessResult, error) {
	result := Run(ctx, ProcessSpec{
		Dir:       dir,
		Argv:      []string{binary, "-describe"},
		Env:       e.EnvVars(),
		Timeout:   timeout,
		MaxStdout: 1 << 20,
		MaxStderr: 1 << 20,
	})
	if !result.Successful() {
		return Signature{}, result, fmt.Errorf("harness describe failed: %s", result.Diagnostic())
	}
	signature, err := ParseSignature(result.Stdout)
	if err != nil {
		return Signature{}, result, err
	}
	return signature, result, nil
}

// ModulePath reads the module path from <snapshot>/go.mod.
func ModulePath(snapshot string) (string, error) {
	path := filepath.Join(snapshot, "go.mod")
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("snapshot %s has no go.mod: %w", snapshot, err)
	}
	if info.Size() > 1<<20 {
		return "", fmt.Errorf("snapshot go.mod exceeds 1 MiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if index := strings.Index(line, "//"); index >= 0 {
			line = strings.TrimSpace(line[:index])
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			module := strings.Trim(fields[1], "\"`")
			if module == "" {
				return "", fmt.Errorf("snapshot go.mod declares an empty module path")
			}
			return module, nil
		}
	}
	return "", fmt.Errorf("snapshot go.mod does not declare a module path")
}

// PackageDir resolves a package import path inside a snapshot and verifies that
// it stays within the snapshot.
func PackageDir(snapshot, modulePath, packagePath string) (string, error) {
	if strings.TrimSpace(packagePath) == "" {
		return "", fmt.Errorf("package import path is required")
	}
	var relative string
	if packagePath == modulePath {
		relative = "."
	} else {
		prefix := modulePath + "/"
		if !strings.HasPrefix(packagePath, prefix) {
			return "", fmt.Errorf("package %s is outside module %s and cannot be instrumented", packagePath, modulePath)
		}
		relative = strings.TrimPrefix(packagePath, prefix)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("package %s escapes the snapshot", packagePath)
	}
	dir := filepath.Join(snapshot, clean)
	resolved, err := filepath.Rel(snapshot, dir)
	if err != nil || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("package %s escapes the snapshot", packagePath)
	}
	snapshotResolved := snapshot
	if evaluated, evalErr := filepath.EvalSymlinks(snapshot); evalErr == nil {
		snapshotResolved = evaluated
	}
	dirResolved := dir
	if evaluated, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
		dirResolved = evaluated
	}
	resolved, err = filepath.Rel(snapshotResolved, dirResolved)
	if err != nil || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("package %s resolves outside the snapshot", packagePath)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("package directory %s does not exist: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("package path %s is not a directory", dir)
	}
	return dir, nil
}

// GoVersion reports the toolchain version used by this process. It is recorded
// in harness digests so evidence can be tied to a toolchain.
func GoVersion() string {
	return runtime.Version()
}
