//go:build !unix

package harness

import "os/exec"

// CommandContext kills the CLI; WaitDelay also bounds inherited output pipes.
func configureAgentCancellation(cmd *exec.Cmd) {}

func stopAgentProcesses(cmd *exec.Cmd) {}
