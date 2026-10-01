package ai

import "context"

// ChatJSONWithRetry runs a JSON-mode chat and, when the output fails the
// caller's `valid` check, retries ONCE with a corrective nudge appended.
//
// WHY THIS EXISTS
// ---------------
// Many features ask a model for STRICT JSON (task extraction, agent/workflow
// drafts, board graphs/clusters, memory extraction). Capable cloud models
// comply; smaller self-hosted models occasionally wrap the JSON in prose or a
// ``` fence, or emit a trailing remark — which makes a one-shot parse fail and
// the whole feature error out. A single corrective retry recovers the vast
// majority of those misses, so the same feature stays reliable on a modest
// local model (protecting the self-host story) without changing any prompt.
//
// Behaviour:
//   - Forces JSONMode on.
//   - Records the circuit-breaker outcome of every model call (nil cb is fine).
//   - Returns the FIRST output that passes `valid`; if neither attempt passes,
//     returns the latest raw output so the caller can still run its own parse
//     and emit its own error message.
//   - A transport error on the first call is returned immediately; a transport
//     error on the retry falls back to the first output (best-effort).
//
// Generic across every JSON-extraction feature; callers supply `valid`
// (typically "does my parser succeed on this output").
func ChatJSONWithRetry(
	ctx context.Context,
	llm LLMProvider,
	cb *CircuitBreaker,
	system, user string,
	opts ChatOptions,
	valid func(string) bool,
) (string, error) {
	opts.JSONMode = true
	messages := []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}

	out, err := llm.Chat(ctx, messages, opts)
	if cb != nil {
		cb.RecordResult(err)
	}
	if err != nil {
		return "", err
	}
	if valid == nil || valid(out) {
		return out, nil
	}

	// One corrective retry: small models often just need to be told to drop
	// the prose / fences and return only the JSON.
	retry := []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
		{Role: "assistant", Content: out},
		{Role: "user", Content: "That was not valid. Reply with ONLY the JSON described in the instructions. No prose, no markdown code fences."},
	}
	out2, err2 := llm.Chat(ctx, retry, opts)
	if cb != nil {
		cb.RecordResult(err2)
	}
	if err2 != nil {
		// Retry transport failed; hand back the first output for a final parse.
		return out, nil
	}
	return out2, nil
}
