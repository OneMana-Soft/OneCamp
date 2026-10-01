package ai

import "testing"

// TestIsReasoningModel verifies the reasoning-family detection that controls
// whether temperature/top_p are omitted (those families 400 on a custom
// temperature). It must match dated/sized variants but never trip on
// unrelated ids, and must only apply to the first-party OpenAI provider.
func TestIsReasoningModel(t *testing.T) {
	openai := &OpenAIProvider{kind: ProviderOpenAI}

	reasoning := []string{
		"o1", "o1-mini", "o1-preview",
		"o3", "o3-mini", "o3-mini-2025-01-31",
		"o4", "o4-mini",
		"gpt-5", "gpt-5-mini", "gpt-5-chat-latest",
		"O3-MINI", // case-insensitive
		"  o1  ",  // trimmed
	}
	for _, m := range reasoning {
		if !openai.isReasoningModel(m) {
			t.Errorf("expected %q to be detected as a reasoning model", m)
		}
	}

	notReasoning := []string{
		"", "gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-3.5-turbo",
		"chatgpt-4o-latest",
		"o1x",      // no '-' boundary, not an exact match
		"o3x-mini", // does not start with "o3-"
		"omni",     // unrelated, starts with 'o' but not a family id
		"text-embedding-3-large",
	}
	for _, m := range notReasoning {
		if openai.isReasoningModel(m) {
			t.Errorf("expected %q NOT to be detected as a reasoning model", m)
		}
	}
}

// TestIsReasoningModel_OnlyFirstPartyOpenAI ensures openai_compatible
// endpoints (vLLM, Groq, …) keep standard chat semantics and are never
// treated as reasoning models, even when a model id collides with the names.
func TestIsReasoningModel_OnlyFirstPartyOpenAI(t *testing.T) {
	compatible := &OpenAIProvider{kind: ProviderOpenAICompatible}
	for _, m := range []string{"o1", "o3-mini", "gpt-5"} {
		if compatible.isReasoningModel(m) {
			t.Errorf("openai_compatible endpoint should not treat %q as reasoning", m)
		}
	}
}
