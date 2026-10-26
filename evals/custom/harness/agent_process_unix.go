//go:build unix

package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// Allow Codex to shut down its own helper groups before WaitDelay escalates
// and stopAgentProcesses kills remaining members of this process group.
func configureAgentCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// Intercept terminal interruption while the CLI is in a separate process
// group. Re-raise it after Invoke's cleanup rather than allowing a retry.
func agentSignalContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	signals := make(chan os.Signal, 1)
	var watched []os.Signal
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	// Preserve nohup/background-job semantics. Notify with no arguments
	// means all signals, so do not call it when every candidate is ignored.
	if len(watched) > 0 {
		signal.Notify(signals, watched...)
	}
	done := make(chan struct{})
	var received os.Signal
	go func() {
		defer close(done)
		select {
		case received = <-signals:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(signals)
		cancel()
		<-done
		if received == nil {
			select {
			case received = <-signals:
			default:
			}
		}
		if sig, ok := received.(syscall.Signal); ok {
			signal.Reset(sig)
			_ = syscall.Kill(os.Getpid(), sig)
			os.Exit(128 + int(sig)) // fallback; never return to the retry loop
		}
	}
}

// Stop background commands even when the CLI exits before its deadline.
func stopAgentProcesses(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
