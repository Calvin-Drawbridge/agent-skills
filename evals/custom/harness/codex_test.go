package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dash0hq/agent-skills/evals/custom/internal/testutil"
	"github.com/stretchr/testify/require"
)

const codexSkillText = "---\nname: otel-ottl\ndescription: TEST-only skill\n---\nRead the rules."
const codexStarted = `{"type":"thread.started","thread_id":"TEST-thread"}`
const codexTurn = `{"type":"turn.started"}`
const codexCompleted = `{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":100,"output_tokens":30}}`

func codexRead(output, status string, exit any) string {
	b, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{
		"id": "TEST-read", "type": "command_execution", "command": "cat SKILL.md",
		"status": status, "exit_code": exit, "aggregated_output": output,
	}})
	return string(b)
}

func parseCodex(lines ...string) *AgentResult {
	res := &AgentResult{}
	p := &codexParser{res: res, skill: codexSkillText}
	for _, line := range lines {
		p.feed([]byte(line))
	}
	return res
}

func TestCodexParserRequiresSkillContentFromSuccessfulTool(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		loaded     bool
	}{
		{"complete skill", codexRead(codexSkillText+"\n", "completed", 0), true},
		{"path only", codexRead("/tmp/.agents/skills/otel-ottl/SKILL.md", "completed", 0), false},
		{"partial read", codexRead("Read the rules.", "completed", 0), false},
		{"wrong skill", codexRead(strings.ReplaceAll(codexSkillText, "otel-ottl", "otel-collector"), "completed", 0), false},
		{"failed read", codexRead(codexSkillText, "failed", 1), false},
		{"missing exit code", codexRead(codexSkillText, "completed", nil), false},
		{"assistant claim", `{"type":"item.completed","item":{"type":"agent_message","text":"I read SKILL.md"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseCodex(codexStarted, codexTurn, tc.line, codexCompleted)
			require.Equal(t, tc.loaded, r.SkillLoaded)
			class, _ := classifyAgentRun(r)
			if tc.loaded {
				require.Equal(t, ClassNone, class)
			} else {
				require.Equal(t, ClassInfra, class)
			}
		})
	}
}

func TestCodexParserTerminalEventsAndUsage(t *testing.T) {
	read := codexRead(codexSkillText, "completed", 0)
	for _, tc := range []struct {
		name  string
		lines []string
		class FailureClass
	}{
		{"success", []string{codexStarted, codexTurn, read, codexCompleted}, ClassNone},
		{"truncated stream", []string{codexStarted, codexTurn, read}, ClassInfra},
		{"missing start", []string{read, codexCompleted}, ClassInfra},
		{"duplicate completion", []string{codexStarted, codexTurn, read, codexCompleted, codexCompleted}, ClassInfra},
		{"malformed event", []string{codexStarted, codexTurn, "not json", read, codexCompleted}, ClassInfra},
		{"unknown event tolerated", []string{codexStarted, codexTurn, `{"type":"future.metadata"}`, read, codexCompleted}, ClassNone},
		{"recoverable transport error", []string{codexStarted, codexTurn, `{"type":"error","message":"API error: 429; retrying"}`, read, codexCompleted}, ClassNone},
		{"terminal API failure", []string{codexStarted, codexTurn, `{"type":"turn.failed","error":{"message":"HTTP 401 invalid API key"}}`}, ClassInfra},
		{"quota failure", []string{codexStarted, codexTurn, `{"type":"turn.failed","error":{"message":"HTTP 429 quota exceeded"}}`}, ClassInfra},
		{"unrecovered stream error", []string{codexStarted, codexTurn, `{"type":"error","message":"connection failed"}`}, ClassInfra},
		{"benign overload text", []string{codexStarted, codexTurn, codexRead(codexSkillText+"\nAPI Error: 429", "completed", 0), codexCompleted}, ClassNone},
		{"negative usage", []string{codexStarted, codexTurn, read, `{"type":"turn.completed","usage":{"input_tokens":-1}}`}, ClassInfra},
		{"missing usage fields", []string{codexStarted, codexTurn, read, `{"type":"turn.completed","usage":{}}`}, ClassInfra},
		{"usage absent", []string{codexStarted, codexTurn, read, `{"type":"turn.completed"}`}, ClassNone},
		{"error after completion", []string{codexStarted, codexTurn, read, codexCompleted, `{"type":"error","message":"fatal"}`}, ClassInfra},
		{"completion after failure", []string{codexStarted, codexTurn, read, `{"type":"turn.failed","error":{"message":"quota"}}`, codexCompleted}, ClassInfra},
		{"second thread", []string{codexStarted, codexTurn, read, codexStarted, codexCompleted}, ClassInfra},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseCodex(tc.lines...)
			class, _ := classifyAgentRun(r)
			require.Equal(t, tc.class, class)
		})
	}
	r := parseCodex(codexStarted, codexTurn, read, codexCompleted)
	require.Equal(t, &TokenUsage{InputTokens: 120, CachedInputTokens: 100, OutputTokens: 30}, r.Usage)
	require.False(t, r.CostKnown, "token counts do not supply a dollar cost")
}

func codexStub(t *testing.T, body string) string {
	t.Helper()
	return testutil.WriteStub(t, "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli "+CodexCLIVersion+"'; exit; fi\n"+body)
}

func codexTestAgent(t *testing.T, binary string) (*CodexAgent, Invocation) {
	t.Helper()
	root := t.TempDir()
	skillDir := filepath.Join(root, "skills", string(SkillOTTL))
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(codexSkillText), 0o644))
	return &CodexAgent{Binary: binary, RepoRoot: root, Model: "TEST-model"}, Invocation{
		WorkDir: t.TempDir(), Skill: SkillOTTL, Prompt: "Update the fixture.",
		TranscriptPath: filepath.Join(t.TempDir(), "transcript.jsonl"),
	}
}

func TestCodexInvokeIsolatesConfigAndCapturesEvidence(t *testing.T) {
	for _, key := range []string{"HOME", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY"} {
		t.Setenv(key, "/TEST-personal-configuration")
	}
	t.Setenv("CODEX_API_KEY", "TEST-api-key")
	capture := t.TempDir()
	body := fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\ncat > %s\nenv > %s\n", shellQuote(filepath.Join(capture, "args")), shellQuote(filepath.Join(capture, "prompt")), shellQuote(filepath.Join(capture, "env")))
	body += testutil.CatBody(codexStarted, codexTurn, codexRead(codexSkillText, "completed", 0), codexCompleted)
	a, inv := codexTestAgent(t, codexStub(t, body))
	inv.Env = map[string]string{EnvOTLPEndpoint: "http://127.0.0.1:12345", EnvOTLPToken: "TEST-relay-token", "HOME": "/TEST-override", "CODEX_HOME": "/TEST-override"}
	r, err := a.Invoke(context.Background(), inv)
	require.NoError(t, err)
	class, detail := classifyAgentRun(r)
	require.Equal(t, ClassNone, class, detail)
	require.Equal(t, "codex", r.Backend)
	require.Equal(t, CodexCLIVersion, r.CLIVersion)
	args, err := os.ReadFile(filepath.Join(capture, "args"))
	require.NoError(t, err)
	require.Contains(t, string(args), "exec\n--json\n--ephemeral\n--ignore-user-config\n")
	require.Contains(t, string(args), "--sandbox\nworkspace-write\n")
	require.Contains(t, string(args), "--model\nTEST-model\n")
	require.NotContains(t, string(args), "--max-turns")
	require.NotContains(t, string(args), "TEST-api-key")
	prompt, err := os.ReadFile(filepath.Join(capture, "prompt"))
	require.NoError(t, err)
	require.Contains(t, string(prompt), "$otel-ottl\n")
	require.Contains(t, string(prompt), inv.Prompt)
	envBytes, err := os.ReadFile(filepath.Join(capture, "env"))
	require.NoError(t, err)
	env := string(envBytes)
	require.NotContains(t, env, "TEST-personal-configuration")
	require.NotContains(t, env, "TEST-override")
	require.NotContains(t, env, "ANTHROPIC_API_KEY=")
	require.NotContains(t, env, "OPENAI_BASE_URL=")
	require.Contains(t, env, "CODEX_API_KEY=TEST-api-key")
	require.Contains(t, env, EnvOTLPToken+"=TEST-relay-token")
	for _, line := range strings.Split(env, "\n") {
		if home, ok := strings.CutPrefix(line, "HOME="); ok {
			require.NoDirExists(t, home, "isolated home must be removed")
		}
	}
	transcript, err := os.ReadFile(inv.TranscriptPath)
	require.NoError(t, err)
	require.Contains(t, string(transcript), codexCompleted)
}

func TestCodexInvokeSavedAuthIsTemporaryAndLocalOnly(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("CODEX_API_KEY", "TEST-must-not-override-subscription")
	capture := filepath.Join(t.TempDir(), "home")
	body := fmt.Sprintf("printf '%%s' \"$HOME\" > %s\ntest -z \"$CODEX_API_KEY\" || exit 9\ntest -f \"$CODEX_HOME/auth.json\" || exit 8\n", shellQuote(capture))
	body += testutil.CatBody(codexStarted, codexTurn, codexRead(codexSkillText, "completed", 0), codexCompleted)
	a, inv := codexTestAgent(t, codexStub(t, body))
	a.AuthFile = filepath.Join(t.TempDir(), "auth.json")
	auth := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"TEST-access-token"}}`)
	require.NoError(t, os.WriteFile(a.AuthFile, auth, 0o600))
	r, err := a.Invoke(context.Background(), inv)
	require.NoError(t, err)
	class, detail := classifyAgentRun(r)
	require.Equal(t, ClassNone, class, detail)
	home, err := os.ReadFile(capture)
	require.NoError(t, err)
	require.NoDirExists(t, string(home))
	original, err := os.ReadFile(a.AuthFile)
	require.NoError(t, err)
	require.Equal(t, auth, original, "original login must not be modified")
	t.Setenv("CI", "true")
	_, err = a.Invoke(context.Background(), inv)
	require.ErrorContains(t, err, "local-only")
}

func TestCodexInvokeRejectsCLIAndStreamFailures(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "TEST-api-key")
	for _, tc := range []struct{ name, body string }{
		{"nonzero after success", testutil.CatBody(codexStarted, codexTurn, codexRead(codexSkillText, "completed", 0), codexCompleted) + "exit 1\n"},
		{"no completion", testutil.CatBody(codexStarted, codexTurn)},
		{"malformed json", testutil.CatBody(codexStarted, codexTurn, "{invalid", codexCompleted)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, inv := codexTestAgent(t, codexStub(t, tc.body))
			r, err := a.Invoke(context.Background(), inv)
			require.NoError(t, err)
			class, _ := classifyAgentRun(r)
			require.Equal(t, ClassInfra, class)
		})
	}
	t.Run("wrong pinned version", func(t *testing.T) {
		a, inv := codexTestAgent(t, testutil.WriteStub(t, "#!/bin/sh\necho 'codex-cli TEST-wrong-version'\n"))
		_, err := a.Invoke(context.Background(), inv)
		require.ErrorContains(t, err, "expected codex-cli")
	})
}

func TestCodexInvokeTimeoutBoundsInheritedPipes(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	a, inv := codexTestAgent(t, codexStub(t, "touch "+shellQuote(started)+"\nsleep 30 &\nwait\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := os.Stat(started); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	start := time.Now()
	r, err := a.Invoke(ctx, inv)
	require.NoError(t, err)
	require.True(t, r.TimedOut)
	class, _ := classifyAgentRun(r)
	require.Equal(t, ClassAgentTelemetry, class)
	require.Less(t, time.Since(start), 5*time.Second)
}

func TestCodexRunnerReusesTelemetryAssertionsAndPreservesUnknownCost(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			a, _ := codexTestAgent(t, codexStub(t, testutil.CatBody(codexStarted, codexTurn, codexRead(codexSkillText, "completed", 0), codexCompleted)))
			attrs := map[string]string{"service.name": "checkout"}
			if !valid {
				attrs = nil
			}
			runner := &Runner{RepoRoot: a.RepoRoot, Agent: a, Hooks: sendSpanHooks("GET /checkout", attrs)}
			sc := testScenario()
			sc.Skill = SkillOTTL
			sc.TelemetryTimeout = 200 * time.Millisecond
			v := runner.Run(t, sc)
			require.Equal(t, valid, v.Passed)
			if !valid {
				require.Equal(t, ClassAgentAssert, v.Class)
			}
			require.Nil(t, v.CostUSD, "Codex does not report USD cost")
			require.Equal(t, "codex", v.AgentBackend)
			require.NotNil(t, v.Usage)
			require.Equal(t, int64(120*v.AgentAttempts), v.Usage.InputTokens)
			b, err := json.Marshal(v)
			require.NoError(t, err)
			require.Contains(t, string(b), `"cost_usd":null`)
		})
	}
}

func TestCodexOutputChunkingAndLimits(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "transcript"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	var lines []string
	o := &codexOutput{transcript: f, feed: func(b []byte) { lines = append(lines, string(b)) }}
	for _, chunk := range []string{"{\"type\":", "\"thread.started\"}\n{\"type\":\"turn.", "started\"}"} {
		_, err = o.Write([]byte(chunk))
		require.NoError(t, err)
	}
	o.flush()
	require.Equal(t, []string{`{"type":"thread.started"}`, codexTurn}, lines)
	_, err = o.Write(bytes.Repeat([]byte("x"), 16*1024*1024+1))
	require.ErrorContains(t, err, "exceeds 16 MiB")
}

func TestCopyCodexAuthRejectsOtherCredentialsWithoutLeakingThem(t *testing.T) {
	src := filepath.Join(t.TempDir(), "auth.json")
	dst := filepath.Join(t.TempDir(), "auth.json")
	for _, data := range []string{
		`{"auth_mode":"apikey","OPENAI_API_KEY":"TEST-secret"}`,
		`{"auth_mode":"chatgpt","tokens":{}}`,
		`invalid TEST-secret`,
	} {
		require.NoError(t, os.WriteFile(src, []byte(data), 0o600))
		err := copyCodexAuth(src, dst)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "TEST-secret")
		require.NoFileExists(t, dst)
	}
	require.NoError(t, os.WriteFile(src, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"TEST-secret"}}`), 0o600))
	require.NoError(t, copyCodexAuth(src, dst))
	info, err := os.Stat(dst)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
