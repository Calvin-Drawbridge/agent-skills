//go:build unix

package harness

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Kill the invocation's process group when its deadline expires, including
// shell commands spawned by the CLI. Never signal the harness's own group.
func configureAgentCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// Stop background commands even when the CLI exits before its deadline.
func stopAgentProcesses(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
