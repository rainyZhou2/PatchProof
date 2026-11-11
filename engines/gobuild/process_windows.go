//go:build windows

package gobuild

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

// configureProcessGroup is a no-op on Windows, which has no portable equivalent
// of Unix process groups.
func configureProcessGroup(*exec.Cmd) {}

// killProcessTree terminates the direct child process. Grandchildren may outlive
// cancellation on Windows; the Unix build signals the whole process group.
func killProcessTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// waitProcessGroup is a no-op on Windows: killing the direct child is enough to
// release the files it had open, and there is no group to poll.
func waitProcessGroup(*os.Process, time.Duration) {}
