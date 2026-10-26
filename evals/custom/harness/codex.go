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
	// AuthHome explicitly opts local runs into a dedicated, persistent
	// evaluation login. Empty uses CODEX_API_KEY. Never copy a refresh token
	// into a disposable home: rotation would invalidate the source login.
	AuthHome string
}

func (a *CodexAgent) Invoke(ctx context.Context, inv Invocation) (*AgentResult, error) {
	ctx, finishSignals := agentSignalContext(ctx)
	// Registered first so interruption is re-raised only after child and
	// temporary-directory cleanup. It must not become a retryable timeout.
	defer finishSignals()
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
	if a.AuthHome != "" && (os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "") {
		return nil, fmt.Errorf("codex: saved ChatGPT authentication is local-only")
	}

	home, err := os.MkdirTemp("", "agent-skills-codex-")
	if err != nil {
		return nil, fmt.Errorf("codex: create isolated home: %w", err)
	}
	defer os.RemoveAll(home)
	codexHome := filepath.Join(home, ".codex")
	if a.AuthHome != "" {
		codexHome, err = validateCodexAuthHome(a.AuthHome)
		if err != nil {
			return nil, err
		}
	}
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
	env := codexEnv(home, codexHome, inv.Env)
	if a.AuthHome == "" {
		env = append(env, "CODEX_API_KEY="+os.Getenv("CODEX_API_KEY"))
	}
	binary := a.Binary
	if binary == "" {
		binary = "codex"
	}
	versionCmd := exec.CommandContext(ctx, binary, "--version")
	versionCmd.Env = env
	versionCmd.WaitDelay = 2 * time.Second
	configureAgentCancellation(versionCmd)
	version, err := versionCmd.Output()
	stopAgentProcesses(versionCmd)
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
		"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--color", "never",
		"--sandbox", "workspace-write",
		"--model", a.Model,
		"-c", `approval_policy="never"`,
		"-c", "sandbox_workspace_write.network_access=true",
		"-c", "project_doc_max_bytes=0",
		"-c", `web_search="disabled"`,
		"-c", `model_reasoning_effort="medium"`,
		"-c", `cli_auth_credentials_store="file"`,
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

// Keep evaluation tokens in their own persistent CODEX_HOME so the CLI can
// atomically refresh them itself. Reject aliases of either personal home;
// copying back over a concurrent interactive login is not safe.
func validateCodexAuthHome(home string) (string, error) {
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("codex: EVAL_CODEX_AUTH_HOME must be an absolute path to a dedicated evaluation login")
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("codex: evaluation login unavailable; sign in with CODEX_HOME set to EVAL_CODEX_AUTH_HOME: %w", err)
	}
	homeInfo, err := os.Stat(home)
	if err != nil || !homeInfo.IsDir() {
		return "", fmt.Errorf("codex: EVAL_CODEX_AUTH_HOME must be a directory")
	}
	info, err := os.Lstat(filepath.Join(home, "auth.json"))
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("codex: evaluation auth.json must be a regular file; run codex login with the dedicated CODEX_HOME")
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, personal := range []string{os.Getenv("CODEX_HOME"), filepath.Join(userHome, ".codex")} {
		if personal == "" {
			continue
		}
		personalInfo, err := os.Stat(personal)
		if err == nil && os.SameFile(homeInfo, personalInfo) {
			return "", fmt.Errorf("codex: EVAL_CODEX_AUTH_HOME must be separate from your personal CODEX_HOME; create a dedicated evaluation login")
		}
		personalAuth, err := os.Stat(filepath.Join(personal, "auth.json"))
		if err == nil && os.SameFile(info, personalAuth) {
			return "", fmt.Errorf("codex: evaluation auth.json must not alias your personal login; create a dedicated evaluation login")
		}
	}
	// --ignore-user-config only skips config.toml. Persistent global
	// instructions, memories, or custom skills would contaminate the eval.
	for _, name := range []string{"AGENTS.md", "AGENTS.override.md", "rules", "memories", "memory_summary.md"} {
		if _, err := os.Lstat(filepath.Join(home, name)); !os.IsNotExist(err) {
			return "", fmt.Errorf("codex: evaluation home contains or cannot inspect %s; use a clean dedicated login", name)
		}
	}
	skillsPath := filepath.Join(home, "skills")
	if st, err := os.Lstat(skillsPath); err == nil && !st.IsDir() {
		return "", fmt.Errorf("codex: evaluation skills path must be a directory, not an alias")
	}
	entries, err := os.ReadDir(skillsPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("codex: cannot inspect evaluation skills: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != ".system" || !entry.IsDir() {
			return "", fmt.Errorf("codex: evaluation home contains custom skills; use a clean dedicated login")
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return "", fmt.Errorf("codex: read evaluation login: %w", err)
	}
	var auth struct {
		AuthMode string `json:"auth_mode"`
		Tokens   *struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil || auth.AuthMode != "chatgpt" || auth.Tokens == nil || auth.Tokens.AccessToken == "" {
		return "", fmt.Errorf("codex: auth.json is not a ChatGPT login; run codex login with the dedicated CODEX_HOME")
	}
	return home, nil
}

// codexEnv starts with a small OS/tooling allowlist. In particular it does not
// inherit personal OTLP exporters, provider URLs, credentials, or Codex config.
// Invocation env supplies only the fixture contract; isolation keys win last.
func codexEnv(home, codexHome string, extra map[string]string) []string {
	env := map[string]string{}
	for _, key := range []string{
		"PATH", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "LANG", "LC_ALL",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "CODEX_CA_CERTIFICATE",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	} {
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
	env["CODEX_HOME"] = codexHome
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
		if p.res.SawInit && p.started && !p.res.SawResult && ev.Item.Type == "command_execution" && ev.Item.Status == "completed" && ev.Item.ExitCode != nil && *ev.Item.ExitCode == 0 && p.skill != "" && strings.Contains(strings.ReplaceAll(ev.Item.Output, "\r\n", "\n"), strings.ReplaceAll(p.skill, "\r\n", "\n")) {
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
