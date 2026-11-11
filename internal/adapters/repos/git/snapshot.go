package git

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
)

type Config struct {
	WorkDir               string
	GitBinary             string
	AllowedHosts          []string
	AllowFileRepositories bool
	AllowInsecureHTTP     bool
	AllowFullFetch        bool
	MaxRepositoryBytes    int64
	MaxFiles              int
	MaxFileBytes          int64
	Timeout               time.Duration
	SSHCommand            string
	Logger                *slog.Logger
}

type Snapshotter struct {
	config Config
	store  ports.ArtifactStore
	clock  ports.Clock
	logger *slog.Logger
}

func New(config Config, store ports.ArtifactStore, clock ports.Clock, logger *slog.Logger) (*Snapshotter, error) {
	if store == nil {
		return nil, fmt.Errorf("git snapshotter: artifact store is required")
	}
	if config.GitBinary == "" {
		config.GitBinary = "git"
	}
	if config.WorkDir == "" {
		config.WorkDir = os.TempDir()
	}
	if config.MaxRepositoryBytes <= 0 {
		config.MaxRepositoryBytes = 2 << 30
	}
	if config.MaxFiles <= 0 {
		config.MaxFiles = 200000
	}
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = 256 << 20
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Minute
	}
	if clock == nil {
		clock = systemClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Snapshotter{config: config, store: store, clock: clock, logger: logger}, nil
}

func (s *Snapshotter) Health(ctx context.Context) error {
	command := exec.CommandContext(ctx, s.config.GitBinary, "--version")
	command.Env = s.gitEnv(s.config.WorkDir)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git binary check failed: %w: %s", err, truncate(string(output), 512))
	}
	return nil
}

func (s *Snapshotter) Snapshot(ctx context.Context, request ports.SnapshotRequest) (ports.SnapshotResult, error) {
	if err := request.Repository.Validate(); err != nil {
		return ports.SnapshotResult{}, err
	}
	if err := s.validateRepository(request.Repository.URI); err != nil {
		return ports.SnapshotResult{}, err
	}
	timeout := s.config.Timeout
	if request.Budget.WallSeconds > 0 {
		budget := time.Duration(request.Budget.WallSeconds) * time.Second
		if budget < timeout {
			timeout = budget
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workRoot, err := os.MkdirTemp(s.config.WorkDir, "patchproof-git-")
	if err != nil {
		return ports.SnapshotResult{}, fmt.Errorf("create snapshot workspace: %w", err)
	}
	defer os.RemoveAll(workRoot)
	repositoryDir := filepath.Join(workRoot, "repo")
	snapshotDir := filepath.Join(workRoot, "snapshot")
	if err := os.MkdirAll(repositoryDir, 0o750); err != nil {
		return ports.SnapshotResult{}, err
	}
	if err := os.MkdirAll(snapshotDir, 0o750); err != nil {
		return ports.SnapshotResult{}, err
	}

	if err := s.fetch(ctx, repositoryDir, request.Repository); err != nil {
		return ports.SnapshotResult{}, err
	}
	commit, err := s.resolveCommit(ctx, repositoryDir, request.Repository.Commit)
	if err != nil {
		return ports.SnapshotResult{}, err
	}
	if err := s.extract(ctx, repositoryDir, snapshotDir, commit); err != nil {
		return ports.SnapshotResult{}, err
	}
	manifest, err := s.buildManifest(repositoryDir, snapshotDir, request.Repository, commit)
	if err != nil {
		return ports.SnapshotResult{}, err
	}
	manifest.FetchedAt = s.clock.Now()
	scope := ports.SecurityScope{TenantID: request.TenantID, TrustDomain: "run"}
	if err := scope.Validate(); err != nil {
		return ports.SnapshotResult{}, err
	}
	archive, err := engine.PutDirTarGz(ctx, s.store, scope, snapshotDir)
	if err != nil {
		return ports.SnapshotResult{}, fmt.Errorf("upload snapshot archive: %w", err)
	}
	s.logger.Info("snapshot fetched",
		slog.String("repository", request.Repository.URI),
		slog.String("commit", commit),
		slog.Int("files", manifest.FileCount),
		slog.Int64("bytes", manifest.TotalBytes),
		slog.String("digest", manifest.Digest),
	)
	return ports.SnapshotResult{Manifest: manifest, Archive: archive, ChangedAt: manifest.FetchedAt}, nil
}

func (s *Snapshotter) validateRepository(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return &domain.ValidationError{Field: "repository_uri", Reason: "is not a valid URL"}
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !s.config.AllowInsecureHTTP {
			return &domain.ValidationError{Field: "repository_uri", Reason: "http is not allowed"}
		}
	case "ssh":
		if parsed.User != nil {
			if _, hasPassword := parsed.User.Password(); hasPassword {
				return &domain.ValidationError{Field: "repository_uri", Reason: "embedded credentials are not allowed"}
			}
		}
	case "file":
		if !s.config.AllowFileRepositories {
			return &domain.ValidationError{Field: "repository_uri", Reason: "file repositories are not allowed"}
		}
		return nil
	default:
		return &domain.ValidationError{Field: "repository_uri", Reason: "unsupported scheme " + scheme}
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return &domain.ValidationError{Field: "repository_uri", Reason: "embedded credentials are not allowed"}
		}
	}
	host := parsed.Hostname()
	if host == "" {
		return &domain.ValidationError{Field: "repository_uri", Reason: "missing host"}
	}
	if len(s.config.AllowedHosts) > 0 && !hostAllowed(host, s.config.AllowedHosts) {
		return &domain.ValidationError{Field: "repository_uri", Reason: "host is not in the repository allowlist"}
	}
	if scheme == "https" || scheme == "ssh" {
		addresses, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
		if err != nil {
			return &domain.ValidationError{Field: "repository_uri", Reason: "cannot resolve repository host"}
		}
		for _, address := range addresses {
			if isPrivateAddress(address.IP) {
				return &domain.ValidationError{Field: "repository_uri", Reason: "repository host resolves to a private address"}
			}
		}
	}
	return nil
}

func hostAllowed(host string, allowed []string) bool {
	for _, pattern := range allowed {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:]
			if strings.HasSuffix(strings.ToLower(host), suffix) {
				return true
			}
			continue
		}
		if strings.EqualFold(host, pattern) {
			return true
		}
	}
	return false
}

func isPrivateAddress(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

func (s *Snapshotter) fetch(ctx context.Context, repositoryDir string, ref domain.RepositoryRef) error {
	if err := s.runGit(ctx, repositoryDir, "init", "--quiet"); err != nil {
		return err
	}
	if err := s.runGit(ctx, repositoryDir, "remote", "add", "origin", ref.URI); err != nil {
		return err
	}
	if err := s.runGit(ctx, repositoryDir, "config", "core.hooksPath", "/dev/null"); err != nil {
		return err
	}
	shallow := []string{"fetch", "--quiet", "--no-tags", "--depth=1", "origin", ref.Commit}
	if err := s.runGit(ctx, repositoryDir, shallow...); err == nil {
		return nil
	}
	if !s.config.AllowFullFetch {
		return &domain.ValidationError{Field: "commit", Reason: "commit cannot be fetched shallowly; full fetch is disabled"}
	}
	if err := s.runGit(ctx, repositoryDir, "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return fmt.Errorf("fetch repository: %w", err)
	}
	return nil
}

func (s *Snapshotter) resolveCommit(ctx context.Context, repositoryDir, requested string) (string, error) {
	output, err := s.gitOutput(ctx, repositoryDir, "rev-parse", "--verify", requested+"^{commit}")
	if err != nil {
		output, err = s.gitOutput(ctx, repositoryDir, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	}
	if err != nil {
		return "", &domain.ValidationError{Field: "commit", Reason: "commit is not present in the fetched repository"}
	}
	commit := strings.TrimSpace(string(output))
	if len(commit) != 40 && len(commit) != 64 {
		return "", &domain.ValidationError{Field: "commit", Reason: "unexpected commit identifier"}
	}
	return commit, nil
}

func (s *Snapshotter) extract(ctx context.Context, repositoryDir, snapshotDir, commit string) error {
	command := exec.CommandContext(ctx, s.config.GitBinary, "archive", "--format=tar", commit)
	command.Dir = repositoryDir
	command.Env = s.gitEnv(repositoryDir)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	command.Stderr = &limitedWriter{builder: &stderr, limit: 64 << 10}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start git archive: %w", err)
	}
	extractErr := extractTar(stdout, snapshotDir, s.config.MaxRepositoryBytes, s.config.MaxFiles, s.config.MaxFileBytes)
	if extractErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return extractErr
	}
	if err := command.Wait(); err != nil {
		return fmt.Errorf("git archive failed: %w: %s", err, truncate(stderr.String(), 2048))
	}
	return nil
}

func (s *Snapshotter) buildManifest(repositoryDir, snapshotDir string, ref domain.RepositoryRef, commit string) (domain.SnapshotManifest, error) {
	manifest := domain.SnapshotManifest{
		Commit:     commit,
		Repository: ref.URI,
		Files:      make([]domain.SnapshotFile, 0, 1024),
	}
	walkErr := filepath.WalkDir(snapshotDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == snapshotDir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(snapshotDir, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		digest, size, err := hashFile(path)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, domain.SnapshotFile{
			Path:       filepath.ToSlash(relative),
			Digest:     digest,
			Size:       size,
			Mode:       fmt.Sprintf("%04o", info.Mode().Perm()),
			Executable: info.Mode()&0o111 != 0,
		})
		manifest.TotalBytes += size
		if manifest.TotalBytes > s.config.MaxRepositoryBytes {
			return fmt.Errorf("snapshot exceeds repository size limit")
		}
		return nil
	})
	if walkErr != nil {
		return domain.SnapshotManifest{}, walkErr
	}
	if len(manifest.Files) > s.config.MaxFiles {
		return domain.SnapshotManifest{}, fmt.Errorf("snapshot exceeds file count limit")
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	manifest.FileCount = len(manifest.Files)
	if data, err := os.ReadFile(filepath.Join(snapshotDir, ".gitmodules")); err == nil {
		manifest.Submodules = parseSubmodulePaths(string(data))
	}
	digest, err := evidence.CanonicalDigest(struct {
		Repository string                `json:"repository"`
		Commit     string                `json:"commit"`
		Files      []domain.SnapshotFile `json:"files"`
	}{Repository: ref.URI, Commit: commit, Files: manifest.Files})
	if err != nil {
		return domain.SnapshotManifest{}, err
	}
	manifest.Digest = digest
	_ = repositoryDir
	return manifest, nil
}

func (s *Snapshotter) runGit(ctx context.Context, dir string, args ...string) error {
	_, err := s.gitOutput(ctx, dir, args...)
	return err
}

func (s *Snapshotter) gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, s.config.GitBinary, args...)
	command.Dir = dir
	command.Env = s.gitEnv(dir)
	var stderr strings.Builder
	command.Stderr = &limitedWriter{builder: &stderr, limit: 64 << 10}
	output, err := command.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git operation timed out")
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, truncate(stderr.String(), 1024))
	}
	return output, nil
}

func (s *Snapshotter) gitEnv(home string) []string {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ASKPASS=/bin/true",
		"GIT_LFS_SKIP_SMUDGE=1",
		"TZ=UTC",
	}
	if s.config.SSHCommand != "" {
		environment = append(environment, "GIT_SSH_COMMAND="+s.config.SSHCommand)
	}
	return environment
}

type limitedWriter struct {
	builder *strings.Builder
	limit   int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.builder.Len() < w.limit {
		remaining := w.limit - w.builder.Len()
		if len(data) > remaining {
			data = data[:remaining]
		}
		w.builder.Write(data)
	}
	return len(data), nil
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	written, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), written, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func extractTar(reader io.Reader, destination string, maxBytes int64, maxFiles int, maxFileBytes int64) error {
	root, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	tarReader := tar.NewReader(reader)
	links := make(map[string]string)
	files := 0
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		files++
		if files > maxFiles {
			return fmt.Errorf("repository exceeds file count limit")
		}
		name := filepath.Clean(header.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("archive entry escapes the repository root")
		}
		target := filepath.Join(root, name)
		relative, err := filepath.Rel(root, target)
		if err != nil || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("archive entry escapes the repository root")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maxFileBytes {
				return fmt.Errorf("file %q exceeds size limit", relative)
			}
			total += header.Size
			if total > maxBytes {
				return fmt.Errorf("repository exceeds size limit")
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(out, tarReader, header.Size); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			links[relative] = header.Linkname
		default:
			continue
		}
	}
	for relative, target := range links {
		cleaned := filepath.Clean(target)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
			continue
		}
		resolved := filepath.Join(filepath.Dir(relative), cleaned)
		if strings.HasPrefix(resolved, "..") || filepath.IsAbs(resolved) {
			continue
		}
		source := filepath.Join(root, resolved)
		info, err := os.Lstat(source)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(source)
		if err != nil {
			continue
		}
		destinationPath := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
			continue
		}
		_ = os.WriteFile(destinationPath, data, 0o644)
	}
	return nil
}

func parseSubmodulePaths(content string) []string {
	var paths []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "path") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				value := strings.TrimSpace(parts[1])
				if value != "" && !strings.HasPrefix(value, "..") {
					paths = append(paths, value)
				}
			}
		}
	}
	sort.Strings(paths)
	return paths
}
