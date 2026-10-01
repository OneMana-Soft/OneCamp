package business

// Optional self-critique (evaluator-optimizer) pass for an agent's final answer.
//
// The runner already has strong heuristic honesty guards (ground-truth
// ToolsSucceeded/FailedTools, stall/placeholder detection, a truthfulness
// directive). This adds a MODEL-DRIVEN verification step: before a tool-using
// run's final answer is posted, the model is shown its own draft alongside the
// tool results already in the conversation and asked to keep it as-is if every
// claim is supported, or correct it if not. This is Anthropic's
// evaluator-optimizer pattern and catches a confident-but-unsupported answer
// that the regex guards can't.
//
// It is OFF by default because it costs one extra model call per verified run —
// a real concern on a metered/free-tier provider. Enable it per deployment with
// AI_AGENT_VERIFY=true, ideally on a capable model (a weak model can "correct" a
// good answer into a worse one). It only runs when a tool actually succeeded
// (there is something to verify against) and the breaker/budget allow, and it is
// best-effort: any error leaves the draft unchanged, so verification can never
// turn a good answer into a failed run.

import (
	"os"
	"strings"
)

// agentVerifyEnabled reports whether the optional self-critique pass is on.
// Default OFF; set AI_AGENT_VERIFY=true|1|on|yes to enable (no logic redeploy).
func agentVerifyEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AI_AGENT_VERIFY")))
	return v == "true" || v == "1" || v == "on" || v == "yes"
}

// buildVerifyPrompt renders the self-critique user turn: it asks the model to
// return the draft unchanged when fully supported, or a corrected version
// otherwise, and to add no new unsupported claims and no tool calls. Pure +
// DB-free so it is unit-testable and model-agnostic.
func buildVerifyPrompt(draft string) string {
	return "Before this reply is sent, verify it against the tool results above. " +
		"If EVERY claim in the draft is supported by those tool results, reply with the draft EXACTLY as-is. " +
		"If any claim is unsupported, wrong, or contradicts a tool result, reply with a corrected version. " +
		"Do NOT add new claims the tool results don't support, do NOT call any tool, and reply with ONLY the answer text (no preamble).\n\n" +
		"Draft reply:\n\"\"\"\n" + strings.TrimSpace(draft) + "\n\"\"\""
}
