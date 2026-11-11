//go:build !unix

package harness

import (
	"os"
	"os/exec"
)

func configureProcess(cmd *exec.Cmd) {
	_ = cmd
}

func killProcessGroup(process *os.Process) {
	if process == nil {
		return
	}
	_ = process.Kill()
}
