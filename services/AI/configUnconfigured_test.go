package ai

import (
	"os"
	"testing"
)

// A hosted provider selected without a key must STAY that provider.
//
// The old behaviour rewrote it to ollama. On a bring-your-own-key deployment
// that has no local engine, the operator's missing key then surfaced as a
// connection error to an Ollama host they never configured; on a deployment that
// does run one, prompts went to a model they had not chosen. Both are worse than
// refusing, and choosing a provider is frequently a data-handling decision, so
// the substitution is the failure this test exists to prevent coming back.
func TestHostedProviderWithoutKeyIsNotSilentlyRewritten(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		wantKind ProviderType
	}{
		{"openai without key", "openai", ProviderOpenAI},
		{"anthropic without key", "anthropic", ProviderAnthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCleanAIEnv(t)
			t.Setenv("AI_PROVIDER", tc.provider)

			cfg := LoadAIConfig()

			if cfg.Chat.Kind != tc.wantKind {
				t.Fatalf("chat provider was rewritten to %q; want %q kept as selected",
					cfg.Chat.Kind, tc.wantKind)
			}
		})
	}
}

// withCleanAIEnv clears every AI variable LoadAIConfig reads, so a test asserts
// against the shipped defaults rather than against whatever the developer or CI
// runner happens to export.
func withCleanAIEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AI_PROVIDER", "AI_ENABLED",
		"OPENAI_API_KEY", "OPENAI_MODEL", "OPENAI_EMBEDDING_MODEL",
		"ANTHROPIC_API_KEY", "ANTHROPIC_MODEL",
		"OLLAMA_HOST", "OLLAMA_MODEL", "OLLAMA_EMBEDDING_MODEL",
	} {
		if _, ok := os.LookupEnv(k); ok {
			t.Setenv(k, "")
		}
	}
}
