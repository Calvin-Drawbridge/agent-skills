//go:build !unix

package harness

import (
	"context"
	"os/exec"
)

// CommandContext kills the CLI; WaitDelay also bounds inherited output pipes.
func configureAgentCancellation(cmd *exec.Cmd) {}

func stopAgentProcesses(cmd *exec.Cmd) {}

func agentSignalContext(ctx context.Context) (context.Context, func()) {
	return ctx, func() {}
}
