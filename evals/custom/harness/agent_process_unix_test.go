//go:build unix

package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise real process trees without a model, credentials, or a live CLI.
func TestCodexProcessHelper(t *testing.T) {
	role := os.Getenv("EVAL_PROCESS_ROLE")
	if role == "" {
		return
	}
	capture := os.Getenv("EVAL_PROCESS_CAPTURE")
	mode := os.Getenv("EVAL_PROCESS_MODE")
	if role == "pass-at-deadline" {
		a, _ := codexTestAgent(t, codexStub(t, "printf '%s\\n' "+shellQuote(codexStarted)+" "+shellQuote(codexTurn)+" "+shellQuote(codexRead(codexSkillText, "completed", 0))+" "+shellQuote(codexCompleted)+"\n"))
		r := &Runner{RepoRoot: a.RepoRoot, Agent: a, Hooks: FixtureHooks{Run: func(ctx context.Context, _ string, env map[string]string) error {
			<-ctx.Done() // The completed fixture's last telemetry arrives at the boundary.
			return sendTestSpan(context.Background(), env, "GET /checkout", map[string]string{"service.name": "checkout"})
		}}}
		sc := testScenario()
		sc.Skill = SkillOTTL
		v := r.Run(t, sc)
		require.True(t, v.Passed, v.Detail)
		require.Equal(t, ClassNone, v.Class)
		return
	}
	if role == "runner" {
		a, inv := processTestAgent(t, capture, "cancel")
		if mode == "deadline" {
			r := &Runner{RepoRoot: a.RepoRoot, Agent: a}
			sc := testScenario()
			sc.Skill = SkillOTTL
			v := r.Run(t, sc)
			require.Equal(t, ClassInfraFail, v.Class)
			require.True(t, v.NonSkillAttributable)
			require.Contains(t, v.Detail, "Go test deadline")
			require.Zero(t, v.InfraRetries, "an exhausted outer budget must not retry")
			return
		}
		_, _ = a.Invoke(context.Background(), inv)
		t.Fatal("interruption must terminate after cleanup, never resume the caller")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	child := exec.Command("sleep", "30")
	if mode == "graceful" {
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	require.NoError(t, child.Start())
	require.NoError(t, os.WriteFile(filepath.Join(capture, "child"), []byte(strconv.Itoa(child.Process.Pid)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(capture, "cli"), []byte(strconv.Itoa(os.Getpid())), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(capture, "home"), []byte(os.Getenv("HOME")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(capture, "ready"), nil, 0o600))
	if mode == "normal" {
		fmt.Println(codexStarted)
		fmt.Println(codexTurn)
		fmt.Println(codexRead(codexSkillText, "completed", 0))
		fmt.Println(codexCompleted)
		os.Exit(0) // Deliberately leave a same-group background child.
	}
	<-signals
	if mode == "graceful" {
		_ = child.Process.Kill()
		_ = child.Wait()
		os.Exit(0)
	}
	// Ignore graceful shutdown to exercise WaitDelay and forced group cleanup.
	select {}
}

func processTestAgent(t *testing.T, capture, mode string) (*CodexAgent, Invocation) {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	body := "EVAL_PROCESS_ROLE=cli EVAL_PROCESS_CAPTURE=" + shellQuote(capture) + " EVAL_PROCESS_MODE=" + shellQuote(mode) + " exec " + shellQuote(binary) + " -test.run='^TestCodexProcessHelper$'\n"
	a, inv := codexTestAgent(t, codexStub(t, body))
	t.Cleanup(func() {
		for _, name := range []string{"child", "cli"} {
			if pid := recordedPID(capture, name); pid > 0 && processRunning(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return a, inv
}

func recordedPID(capture, name string) int {
	b, _ := os.ReadFile(filepath.Join(capture, name))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func processRunning(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) == syscall.ESRCH {
		return false
	}
	if runtime.GOOS == "linux" {
		// A killed, reparented child may await init's reaper; it cannot run.
		stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && strings.HasPrefix(string(stat)[i+1:], " Z") {
			return false
		}
	}
	return true
}

func requireProcessCleanup(t *testing.T, capture string) {
	t.Helper()
	for _, name := range []string{"child", "cli"} {
		pid := recordedPID(capture, name)
		require.Positive(t, pid)
		require.Eventually(t, func() bool { return !processRunning(pid) }, 3*time.Second, 10*time.Millisecond, "%s process %d survived", name, pid)
	}
	home, err := os.ReadFile(filepath.Join(capture, "home"))
	require.NoError(t, err)
	require.NoDirExists(t, string(home))
}

func TestCodexProcessCleanup(t *testing.T) {
	for _, mode := range []string{"normal", "cancel", "graceful"} {
		t.Run(mode, func(t *testing.T) {
			capture := t.TempDir()
			a, inv := processTestAgent(t, capture, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if mode != "normal" {
				go func() {
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							if _, err := os.Stat(filepath.Join(capture, "ready")); err == nil {
								cancel()
								return
							}
						}
					}
				}()
			}
			_, err := a.Invoke(ctx, inv)
			require.NoError(t, err)
			requireProcessCleanup(t, capture)
		})
	}
}

func TestCodexCleanupOnInterruptAndOuterTestDeadline(t *testing.T) {
	for _, mode := range []string{"interrupt", "deadline", "ignored-hangup"} {
		t.Run(mode, func(t *testing.T) {
			capture := t.TempDir()
			// Register PID cleanup for a failed assertion too.
			_, _ = processTestAgent(t, capture, "cancel")
			binary, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.Command(binary, "-test.run=^TestCodexProcessHelper$", "-test.timeout=12s")
			if mode == "ignored-hangup" {
				cmd = exec.Command("/bin/sh", "-c", `trap '' HUP; exec "$@"`, "TEST-helper", binary, "-test.run=^TestCodexProcessHelper$", "-test.timeout=12s")
			}
			cmd.Env = append(os.Environ(), "EVAL_PROCESS_ROLE=runner", "EVAL_PROCESS_CAPTURE="+capture, "EVAL_PROCESS_MODE="+mode)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var output strings.Builder
			cmd.Stdout, cmd.Stderr = &output, &output
			require.NoError(t, cmd.Start())
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			require.Eventually(t, func() bool {
				_, err := os.Stat(filepath.Join(capture, "ready"))
				return err == nil
			}, 8*time.Second, 10*time.Millisecond)
			if mode == "ignored-hangup" {
				require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP))
				require.Never(t, func() bool {
					return !processRunning(recordedPID(capture, "child"))
				}, 300*time.Millisecond, 10*time.Millisecond, "inherited ignored SIGHUP must not cancel the invocation")
			}
			if mode != "deadline" {
				require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGINT))
			}
			err = cmd.Wait()
			if mode != "deadline" {
				require.Error(t, err)
				require.NotContains(t, output.String(), "never resume the caller")
			} else {
				require.NoError(t, err, output.String())
			}
			requireProcessCleanup(t, capture)
		})
	}
}

func TestRunnerPreservesPassAtOuterDeadline(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(binary, "-test.run=^TestCodexProcessHelper$", "-test.timeout=10s")
	cmd.Env = append(os.Environ(), "EVAL_PROCESS_ROLE=pass-at-deadline")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}
