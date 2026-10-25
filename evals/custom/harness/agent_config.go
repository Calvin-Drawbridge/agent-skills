package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrAgentCredentialsUnavailable lets local scenario entrypoints skip without
// accidentally falling back to a different provider or a saved login.
var ErrAgentCredentialsUnavailable = errors.New("agent credentials unavailable")

// AgentFromEnv selects the opt-in backend for both scenario entrypoints.
// Claude remains the default, including its existing version/model pins.
func AgentFromEnv(root string, versions *Versions) (AgentInvoker, error) {
	backend := strings.TrimSpace(os.Getenv("EVAL_AGENT"))
	binary := os.Getenv("EVAL_AGENT_BINARY")
	switch backend {
	case "", "claude":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return nil, fmt.Errorf("%w: ANTHROPIC_API_KEY unset", ErrAgentCredentialsUnavailable)
		}
		return &Agent{Binary: binary, PluginDir: root, Model: versions.EvalModel}, nil
	case "codex":
		model := strings.TrimSpace(os.Getenv("EVAL_CODEX_MODEL"))
		if model == "" {
			return nil, fmt.Errorf("EVAL_AGENT=codex requires an explicit EVAL_CODEX_MODEL")
		}
		a := &CodexAgent{Binary: binary, RepoRoot: root, Model: model}
		switch auth := strings.TrimSpace(os.Getenv("EVAL_CODEX_AUTH")); auth {
		case "", "api":
			if os.Getenv("CODEX_API_KEY") == "" {
				return nil, fmt.Errorf("%w: CODEX_API_KEY unset (for local subscription runs, explicitly select EVAL_CODEX_AUTH=chatgpt)", ErrAgentCredentialsUnavailable)
			}
		case "chatgpt":
			if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
				return nil, fmt.Errorf("EVAL_CODEX_AUTH=chatgpt is local-only; use API authentication on a trusted CI runner")
			}
			home := os.Getenv("CODEX_HOME")
			if home == "" {
				userHome, err := os.UserHomeDir()
				if err != nil {
					return nil, fmt.Errorf("locate Codex login: %w", err)
				}
				home = filepath.Join(userHome, ".codex")
			}
			a.AuthFile = filepath.Join(home, "auth.json")
			if _, err := os.Stat(a.AuthFile); err != nil {
				return nil, fmt.Errorf("%w: local Codex auth.json unavailable; run codex login first", ErrAgentCredentialsUnavailable)
			}
		default:
			return nil, fmt.Errorf("unknown EVAL_CODEX_AUTH %q (use api or chatgpt)", auth)
		}
		return a, nil
	default:
		return nil, fmt.Errorf("unknown EVAL_AGENT %q (use claude or codex)", backend)
	}
}
