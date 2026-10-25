package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// CodexCLIVersion pins the opt-in adapter's command/event contract. Codex is
// not installed or enabled by the existing Claude CI workflows.
const CodexCLIVersion = "0.162.1"

// CodexAgent runs codex exec against the same fixtures as Agent. Only the
// repository's skills are copied into a disposable home. Personal config,
// plugins, MCP servers, and skills are not copied.
type CodexAgent struct {
	Binary   string
	RepoRoot string
	Model    string
	// AuthFile explicitly opts local runs into a saved ChatGPT login. Empty
	// uses CODEX_API_KEY. Credentials stay outside the fixture/evidence tree.
	AuthFile string
}

func (a *CodexAgent) Invoke(ctx context.Context, inv Invocation) (*AgentResult, error) {
	// The CLI has no --max-turns equivalent. Always enforce a wall-clock
	// ceiling, including callers that do not use Runner.
	var cancel context.CancelFunc
	if _, bounded := ctx.Deadline(); bounded {
		ctx, cancel = context.WithCancel(ctx)
	} else {
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
	}
	defer cancel()
	if strings.TrimSpace(a.Model) == "" {
		return nil, fmt.Errorf("codex: an explicit model is required")
	}
	if !slices.Contains(Skills, inv.Skill) {
		return nil, fmt.Errorf("codex: unknown skill %q", inv.Skill)
	}
	if a.AuthFile != "" && (os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "") {
		return nil, fmt.Errorf("codex: saved ChatGPT authentication is local-only")
	}

	home, err := os.MkdirTemp("", "agent-skills-codex-")
	if err != nil {
		return nil, fmt.Errorf("codex: create isolated home: %w", err)
	}
	defer os.RemoveAll(home)
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return nil, err
	}
	skillsDir := filepath.Join(home, ".agents", "skills")
	if err := copyDir(filepath.Join(a.RepoRoot, "skills"), skillsDir); err != nil {
		return nil, fmt.Errorf("codex: copy skills: %w", err)
	}
	skillPath := filepath.Join(skillsDir, string(inv.Skill), "SKILL.md")
	skill, err := os.ReadFile(skillPath)
	if err != nil || len(bytes.TrimSpace(skill)) == 0 {
		return nil, fmt.Errorf("codex: target SKILL.md is missing or empty")
	}
	if a.AuthFile != "" {
		if err := copyCodexAuth(a.AuthFile, filepath.Join(codexHome, "auth.json")); err != nil {
			return nil, err
		}
	}
	env := codexEnv(home, inv.Env)
	if a.AuthFile == "" {
		env = append(env, "CODEX_API_KEY="+os.Getenv("CODEX_API_KEY"))
	}
	binary := a.Binary
	if binary == "" {
		binary = "codex"
	}
	versionCmd := exec.CommandContext(ctx, binary, "--version")
	versionCmd.Env = env
	versionCmd.WaitDelay = 2 * time.Second
	version, err := versionCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codex: check CLI version: %w", err)
	}
	if strings.TrimSpace(string(version)) != "codex-cli "+CodexCLIVersion {
		return nil, fmt.Errorf("codex: expected codex-cli %s; install the pinned CLI before running evaluations", CodexCLIVersion)
	}

	// exec's JSON stream has no skill-catalogue event. Request an observable
	// read and require the complete original SKILL.md in a successful tool
	// result; a path mention or an agent's claim is not loading evidence.
	prompt := fmt.Sprintf("$%s\nBefore editing the fixture, read the full skill instructions with cat %s. Follow those instructions and their referenced rules.\n\n%s",
		inv.Skill, shellQuote(skillPath), inv.Prompt)
	args := []string{
		"exec", "--json", "--ephemeral", "--ignore-user-config",
		"--skip-git-repo-check", "--color", "never",
		"--sandbox", "workspace-write",
		"--model", a.Model,
		"-c", `approval_policy="never"`,
		"-c", "sandbox_workspace_write.network_access=true",
		"-c", "project_doc_max_bytes=0",
		"-c", `web_search="disabled"`,
		"-c", `model_reasoning_effort="medium"`,
		"-c", "shell_environment_policy.experimental_use_profile=false",
		"-c", `shell_environment_policy.exclude=["CODEX_API_KEY","OPENAI_API_KEY","ANTHROPIC_API_KEY"]`,
		"-", // prompt on stdin, never interpreted by a shell
	}
	transcript, err := os.OpenFile(inv.TranscriptPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("codex: create transcript: %w", err)
	}
	defer transcript.Close()
	res := &AgentResult{
		Backend: "codex", Model: a.Model, CLIVersion: CodexCLIVersion, ReasoningEffort: "medium",
		TranscriptPath:  inv.TranscriptPath,
		SkillLoadDetail: "Codex did not return the complete target SKILL.md in a successful command result; skill loading was not verified",
	}
	parser := &codexParser{res: res, skill: strings.TrimSpace(string(skill))}
	output := &codexOutput{transcript: transcript, feed: parser.feed}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = inv.WorkDir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = output
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	configureAgentCancellation(cmd)
	defer stopAgentProcesses(cmd)
	runErr := cmd.Run()
	if cmd.Process == nil {
		return nil, fmt.Errorf("codex: start CLI: %w", runErr)
	}
	output.flush()
	res.ExitErr = runErr
	res.TimedOut = ctx.Err() != nil
	if output.err != nil {
		res.ProtocolError = output.err.Error()
	}
	if res.SawResult && !res.IsError && runErr != nil {
		res.ProtocolError = "CLI exited unsuccessfully after a completion event"
	}
	if stderr.Len() > 0 {
		if err := os.WriteFile(inv.TranscriptPath+".stderr", stderr.Bytes(), 0o600); err != nil {
			return nil, fmt.Errorf("codex: write stderr evidence: %w", err)
		}
		// Only control-plane JSON errors classify overload. Stderr can quote
		// fixture logs and must not turn a benign HTTP 429 into an infra retry.
	}
	return res, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func copyCodexAuth(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("codex: read saved login: %w", err)
	}
	var auth struct {
		AuthMode string `json:"auth_mode"`
		Tokens   *struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil || auth.AuthMode != "chatgpt" || auth.Tokens == nil || auth.Tokens.AccessToken == "" {
		return fmt.Errorf("codex: auth.json is not a ChatGPT login; run codex login first")
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("codex: stage saved login: %w", err)
	}
	return nil
}

// codexEnv starts with a small OS/tooling allowlist. In particular it does not
// inherit personal OTLP exporters, provider URLs, credentials, or Codex config.
// Invocation env supplies only the fixture contract; isolation keys win last.
func codexEnv(home string, extra map[string]string) []string {
	env := map[string]string{}
	for _, key := range []string{"PATH", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	for key, value := range extra {
		if strings.HasPrefix(key, "OTEL_") || strings.HasPrefix(key, "EVAL_") {
			env[key] = value
		}
	}
	env["HOME"] = home
	env["CODEX_HOME"] = filepath.Join(home, ".codex")
	env["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	env["XDG_CACHE_HOME"] = filepath.Join(home, ".cache")
	var out []string
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	slices.Sort(out)
	return out
}

type codexParser struct {
	res     *AgentResult
	skill   string
	started bool
}

func (p *codexParser) feed(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var ev struct {
		Type  string `json:"type"`
		Usage *struct {
			InputTokens           *int64 `json:"input_tokens"`
			CachedInputTokens     *int64 `json:"cached_input_tokens"`
			OutputTokens          *int64 `json:"output_tokens"`
			ReasoningOutputTokens int64  `json:"reasoning_output_tokens"`
		} `json:"usage"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
		Item struct {
			Type     string `json:"type"`
			Status   string `json:"status"`
			ExitCode *int   `json:"exit_code"`
			Output   string `json:"aggregated_output"`
			Text     string `json:"text"`
		} `json:"item"`
	}
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type == "" {
		p.res.ProtocolError = "malformed JSON event"
		return
	}
	switch ev.Type {
	case "thread.started":
		if p.res.SawInit || p.started || p.res.SawResult {
			p.res.ProtocolError = "unexpected additional thread"
		}
		p.res.SawInit = true
	case "turn.started":
		if !p.res.SawInit || p.started || p.res.SawResult {
			p.res.ProtocolError = "unexpected additional turn"
		}
		p.started = true
	case "item.completed":
		if p.res.SawInit && p.started && !p.res.SawResult && ev.Item.Type == "command_execution" && ev.Item.Status == "completed" && ev.Item.ExitCode != nil && *ev.Item.ExitCode == 0 && p.skill != "" && strings.Contains(ev.Item.Output, p.skill) {
			p.res.SkillLoaded = true
		}
	case "turn.completed":
		if !p.res.SawInit || !p.started || p.res.SawResult {
			p.res.ProtocolError = "completion without a unique started thread and turn"
			return
		}
		p.res.SawResult = true
		p.res.ResultSubtype = "success"
		if u := ev.Usage; u != nil {
			if u.InputTokens == nil || u.CachedInputTokens == nil || u.OutputTokens == nil || *u.InputTokens < 0 || *u.CachedInputTokens < 0 || *u.OutputTokens < 0 || u.ReasoningOutputTokens < 0 || *u.CachedInputTokens > *u.InputTokens {
				p.res.ProtocolError = "invalid or incomplete token usage"
				return
			}
			p.res.Usage = &TokenUsage{InputTokens: *u.InputTokens, CachedInputTokens: *u.CachedInputTokens, OutputTokens: *u.OutputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens}
		}
	case "turn.failed":
		if p.res.SawResult {
			p.res.ProtocolError = "multiple terminal events"
			return
		}
		p.res.SawResult = true
		p.res.IsError = true
		// Codex turn.failed is an API/session failure, not a fixture command
		// failure (those are item.completed with a failed status).
		p.res.ResultSubtype = "error_api"
		p.res.ResultText = ev.Error.Message
		p.res.Overloaded = containsOverloadMarker(ev.Error.Message) || overload429529.MatchString(ev.Error.Message)
	case "error":
		// Transport retries emit non-terminal error events. A later
		// turn.completed can recover; a stream ending here still lacks a
		// terminal result and therefore fails closed as infrastructure.
		p.res.ResultText = ev.Message
		if p.res.SawResult {
			p.res.ProtocolError = "error after terminal event"
		}
	}
}

// codexOutput lets os/exec own pipe copying so WaitDelay can close pipes held
// by abandoned children. A Scanner loop before Wait cannot provide that bound.
type codexOutput struct {
	transcript *os.File
	feed       func([]byte)
	pending    []byte
	err        error
}

func (o *codexOutput) Write(data []byte) (int, error) {
	if _, err := o.transcript.Write(data); err != nil {
		o.err = err
		return 0, err
	}
	n := len(data)
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		end := len(data)
		if i >= 0 {
			end = i
		}
		if len(o.pending)+end > 16*1024*1024 {
			o.err = fmt.Errorf("JSON event exceeds 16 MiB")
			return 0, o.err
		}
		o.pending = append(o.pending, data[:end]...)
		if i < 0 {
			break
		}
		o.feed(o.pending)
		o.pending = o.pending[:0]
		data = data[i+1:]
	}
	return n, nil
}

func (o *codexOutput) flush() {
	if len(o.pending) > 0 {
		o.feed(o.pending)
		o.pending = nil
	}
}
