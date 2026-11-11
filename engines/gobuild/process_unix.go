//go:build !windows

package gobuild

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureProcessGroup places the toolchain command in its own process group so
// that cancelling the step also terminates the compiler, linker and test
// processes it spawns.
func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree terminates the command's entire process group. Processes that
// survive cancellation keep writing into the step workspace and race with
// workspace cleanup, so the whole group must be signalled, not only the direct
// child.
func killProcessTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// waitProcessGroup blocks until every member of the process group has exited or
// the timeout elapses. A SIGKILLed process can still finish an in-flight file
// operation before the kernel terminates it, so callers that delete the step
// workspace immediately must wait for the group to drain.
func waitProcessGroup(process *os.Process, timeout time.Duration) {
	if process == nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(-process.Pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}
