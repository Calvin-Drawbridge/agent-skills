package harness

// FailureClass classifies why a scenario attempt (or the terminal verdict)
// failed. The class decides the retry policy and whether the failure is
// attributed to the skill content.
type FailureClass string

// The failure taxonomy.
const (
	// ClassNone means the attempt passed.
	ClassNone FailureClass = ""

	// ClassInfra marks failures that are not attributable to the skill:
	// Anthropic API overload markers (429/529), a CLI crash or non-zero
	// exit without a result event, non-empty plugin_errors in the init
	// event, or a sink/relay/harness error. Infra failures are retried up
	// to MaxInfraAttempts total attempts without consuming the
	// agent-attributable retry.
	ClassInfra FailureClass = "infra"

	// ClassInfraFail is the terminal verdict class after MaxInfraAttempts
	// consecutive-or-not infra failures. It fails the gate but is flagged
	// non-skill-attributable.
	ClassInfraFail FailureClass = "infra-fail"

	// ClassAgentBuild means the agent's output does not build or the
	// fixture fails to run.
	ClassAgentBuild FailureClass = "agent-build"

	// ClassAgentTelemetry means the fixture built but no telemetry reached
	// the sink before the timeout; stalled agent runs killed at the
	// wall-clock budget also carry this class.
	ClassAgentTelemetry FailureClass = "agent-telemetry"

	// ClassAgentAssert means telemetry arrived but the scenario assertions
	// failed on it.
	ClassAgentAssert FailureClass = "agent-assert"
)

// AgentAttributable reports whether the class blames the skill content (and
// therefore follows the retry-once policy) rather than the infrastructure.
func (c FailureClass) AgentAttributable() bool {
	switch c {
	case ClassAgentBuild, ClassAgentTelemetry, ClassAgentAssert:
		return true
	default:
		return false
	}
}

// Retry policy limits.
const (
	// MaxInfraAttempts is the total number of attempts allowed when every
	// failure is infra-classified; the MaxInfraAttempts-th infra failure
	// makes the verdict terminal with ClassInfraFail.
	MaxInfraAttempts = 3
	// MaxAgentAttempts is the total number of agent-attributable attempts:
	// each agent-attributable failure class is retried once.
	MaxAgentAttempts = 2
)

// Evidence points at the artifacts that substantiate a verdict.
type Evidence struct {
	// TranscriptPaths lists the agent transcript files (stream-json, one
	// per attempt, in attempt order). Agent stderr, when non-empty, sits
	// next to each transcript with a ".stderr" suffix.
	TranscriptPaths []string `json:"transcript_paths,omitempty"`
	// TelemetryPaths lists the received-telemetry JSONL files
	// (traces.jsonl, metrics.jsonl, logs.jsonl) that exist for the last
	// attempt.
	TelemetryPaths []string `json:"telemetry_paths,omitempty"`
}

// TokenUsage is the usage reported by Codex for a completed turn. Cached input
// is a subset of input, not an additional token charge. Counts do not imply a
// dollar price, including when running with a ChatGPT subscription.
type TokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens,omitempty"`
}

// Verdict is the terminal outcome of running one scenario, after the retry
// policy has been applied.
type Verdict struct {
	// ScenarioID names the scenario this verdict belongs to.
	ScenarioID      string `json:"scenario_id"`
	AgentBackend    string `json:"agent_backend,omitempty"`
	Model           string `json:"model,omitempty"`
	CLIVersion      string `json:"cli_version,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Passed reports whether the scenario ultimately passed.
	Passed bool `json:"passed"`
	// Class is the terminal failure class; empty when Passed.
	Class FailureClass `json:"class,omitempty"`
	// Detail is a human-readable description of the terminal failure.
	Detail string `json:"detail,omitempty"`
	// AgentAttempts counts the agent-attributable attempts consumed,
	// including a passing one; infra retries are not counted here.
	AgentAttempts int `json:"agent_attempts"`
	// InfraRetries counts the infra-classified failures that were absorbed
	// without consuming the agent retry.
	InfraRetries int `json:"infra_retries"`
	// Flake is true when the scenario passed only on the agent retry.
	Flake bool `json:"flake"`
	// FlakeNote describes the recorded flake (which class the first
	// attempt failed with).
	FlakeNote string `json:"flake_note,omitempty"`
	// NonSkillAttributable is true for terminal infra failures: the gate
	// fails, but the failure must not be reported as a skill failure.
	NonSkillAttributable bool `json:"non_skill_attributable,omitempty"`
	// CostUSD sums reported cost across attempts. Null means at least one
	// agent invocation did not report cost; setup failures cost nothing.
	CostUSD *float64 `json:"cost_usd"`
	// Usage sums token counts only when every agent invocation reported them.
	Usage *TokenUsage `json:"usage,omitempty"`
	// Evidence points at transcripts and telemetry files.
	Evidence Evidence `json:"evidence"`
}
