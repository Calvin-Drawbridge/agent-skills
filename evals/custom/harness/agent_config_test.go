package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentFromEnvSelectionAndCredentials(t *testing.T) {
	for _, key := range []string{"EVAL_AGENT", "EVAL_AGENT_BINARY", "EVAL_CODEX_MODEL", "EVAL_CODEX_AUTH", "CODEX_API_KEY", "ANTHROPIC_API_KEY", "CI", "GITHUB_ACTIONS"} {
		t.Setenv(key, "")
	}
	versions := &Versions{EvalModel: "TEST-claude-model"}
	_, err := AgentFromEnv("TEST-root", versions)
	require.ErrorIs(t, err, ErrAgentCredentialsUnavailable)
	t.Setenv("ANTHROPIC_API_KEY", "TEST-anthropic-key")
	agent, err := AgentFromEnv("TEST-root", versions)
	require.NoError(t, err)
	require.IsType(t, &Agent{}, agent)
	require.Equal(t, "TEST-claude-model", agent.(*Agent).Model)
	t.Setenv("EVAL_AGENT", "codex")
	_, err = AgentFromEnv("TEST-root", versions)
	require.ErrorContains(t, err, "EVAL_CODEX_MODEL")
	t.Setenv("EVAL_CODEX_MODEL", "TEST-codex-model")
	_, err = AgentFromEnv("TEST-root", versions)
	require.ErrorIs(t, err, ErrAgentCredentialsUnavailable, "must not use Claude's key or automatically consume subscription quota")
	t.Setenv("CODEX_API_KEY", "TEST-codex-key")
	agent, err = AgentFromEnv("TEST-root", versions)
	require.NoError(t, err)
	require.IsType(t, &CodexAgent{}, agent)
	require.Equal(t, "TEST-codex-model", agent.(*CodexAgent).Model)
	t.Setenv("EVAL_AGENT", "typo")
	_, err = AgentFromEnv("TEST-root", versions)
	require.ErrorContains(t, err, "unknown EVAL_AGENT")
}

func TestAgentFromEnvExplicitLocalSubscription(t *testing.T) {
	for _, key := range []string{"CODEX_API_KEY", "CI", "GITHUB_ACTIONS"} {
		t.Setenv(key, "")
	}
	t.Setenv("EVAL_AGENT", "codex")
	t.Setenv("EVAL_CODEX_MODEL", "TEST-model")
	t.Setenv("EVAL_CODEX_AUTH", "chatgpt")
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	_, err := AgentFromEnv("TEST-root", &Versions{})
	require.ErrorIs(t, err, ErrAgentCredentialsUnavailable)
	require.NoError(t, os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"TEST-token"}}`), 0o600))
	agent, err := AgentFromEnv("TEST-root", &Versions{})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "auth.json"), agent.(*CodexAgent).AuthFile)
	t.Setenv("CI", "true")
	_, err = AgentFromEnv("TEST-root", &Versions{})
	require.ErrorContains(t, err, "local-only")
	t.Setenv("CI", "")
	t.Setenv("EVAL_CODEX_AUTH", "invalid")
	_, err = AgentFromEnv("TEST-root", &Versions{})
	require.ErrorContains(t, err, "unknown EVAL_CODEX_AUTH")
}
