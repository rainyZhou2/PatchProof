package gobuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
)

// cappedBuffer is a concurrency-safe writer that keeps at most limit bytes and
// records whether the stream was truncated.
type cappedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

// newCappedBuffer returns a buffer bounded by limit bytes.
func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

// Write implements io.Writer. It always reports a full write so the command
// never observes a broken pipe because of log truncation.
func (b *cappedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(data) <= remaining {
			b.buffer.Write(data)
		} else {
			b.buffer.Write(data[:remaining])
			b.truncated = true
		}
	} else if len(data) > 0 {
		b.truncated = true
	}
	return len(data), nil
}

// String returns the captured bytes.
func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// Truncated reports whether the stream exceeded the cap.
func (b *cappedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// commandOutcome is the result of one toolchain invocation.
type commandOutcome struct {
	stdout          string
	stderr          string
	stdoutTruncated bool
	stderrTruncated bool
	exitCode        int
	timedOut        bool
	startErr        error
}

// runGoCommand executes the go binary directly with bounded output capture.
func runGoCommand(ctx context.Context, goBinary, directory string, environment, args []string) commandOutcome {
	stdout := newCappedBuffer(maxLogBytes)
	stderr := newCappedBuffer(maxLogBytes)
	outcome := commandOutcome{exitCode: -1}
	// #nosec G204 -- arguments are validated and the binary is resolved from PATH.
	command := exec.CommandContext(ctx, goBinary, args...)
	command.Dir = directory
	command.Env = environment
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = 5 * time.Second
	err := command.Run()
	outcome.stdout = stdout.String()
	outcome.stderr = stderr.String()
	outcome.stdoutTruncated = stdout.Truncated()
	outcome.stderrTruncated = stderr.Truncated()
	if err == nil {
		outcome.exitCode = 0
		return outcome
	}
	if ctx.Err() != nil {
		outcome.timedOut = true
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		outcome.exitCode = exitError.ExitCode()
		return outcome
	}
	if ctx.Err() == nil {
		outcome.startErr = err
	}
	return outcome
}

// commandContext derives the command deadline from the step deadline.
func commandContext(parent context.Context, deadline time.Time, margin time.Duration) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(parent)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithDeadline(parent, time.Now().Add(-time.Millisecond))
	}
	budget := remaining - margin
	if budget < remaining/2 {
		budget = remaining / 2
	}
	return context.WithTimeout(parent, budget)
}

// composeLog renders a deterministic log artifact for one command.
func composeLog(header string, args []string, outcome commandOutcome) []byte {
	var builder strings.Builder
	builder.WriteString(header)
	builder.WriteString("\ncommand: go ")
	builder.WriteString(strings.Join(args, " "))
	fmt.Fprintf(&builder, "\nexit_code: %d\ntimed_out: %t\n", outcome.exitCode, outcome.timedOut)
	if outcome.startErr != nil {
		builder.WriteString("start_error: ")
		builder.WriteString(outcome.startErr.Error())
		builder.WriteString("\n")
	}
	builder.WriteString("=== stdout ===\n")
	builder.WriteString(outcome.stdout)
	if outcome.stdoutTruncated {
		fmt.Fprintf(&builder, "\n[stdout truncated at %d bytes]\n", maxLogBytes)
	}
	builder.WriteString("\n=== stderr ===\n")
	builder.WriteString(outcome.stderr)
	if outcome.stderrTruncated {
		fmt.Fprintf(&builder, "\n[stderr truncated at %d bytes]\n", maxLogBytes)
	}
	builder.WriteString("\n")
	return []byte(builder.String())
}

// putLog writes content to a scratch file and uploads it to the CAS.
func putLog(ctx context.Context, request engine.Request, workspace buildWorkspace, name string, content []byte) (domain.CASRef, error) {
	directory := filepath.Join(workspace.cacheRoot, "logs")
	path := filepath.Join(directory, filepath.Base(name))
	if !isWithin(directory, path) {
		return domain.CASRef{}, fmt.Errorf("log path escapes the build workspace")
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return domain.CASRef{}, fmt.Errorf("write log %s: %w", name, err)
	}
	ref, err := engine.PutFile(ctx, request.Store, request.Scope, path)
	_ = os.Remove(path)
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("upload log %s: %w", name, err)
	}
	return ref, nil
}

// normalizeLimits sorts and de-duplicates limitation messages.
func normalizeLimits(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	write := 1
	for read := 1; read < len(out); read++ {
		if out[read] != out[write-1] {
			out[write] = out[read]
			write++
		}
	}
	return out[:write]
}

// firstNonEmptyLine returns the first non-blank line of a stream, bounded.
func firstNonEmptyLine(stream string) string {
	for _, line := range strings.Split(stream, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if len(trimmed) > 240 {
				return trimmed[:237] + "..."
			}
			return trimmed
		}
	}
	return ""
}
