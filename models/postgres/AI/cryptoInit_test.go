package models

import (
	"strings"
	"testing"
)

// The startup probe must refuse a placeholder KEK, in every environment.
//
// Separate from helpers.TestInspectKEK* on purpose. Those prove the classifier recognises a
// placeholder; this proves the probe ACTS on it. The original bug was not a wrong classification — it
// was that nothing asked the question, so the server booted and encrypted provider API keys and MCP
// auth secrets with a value committed to vars/.env.beta.
//
// APP_ENV is left unset in the first case because that is how beta actually runs: the variable appears
// in no env template and no compose file, so the pre-existing production-only guards could never fire
// there. A check that only works when an unset variable is set is not a check.
func TestValidateAIKeyEncryptionRefusesPlaceholder(t *testing.T) {
	const placeholder = "__SET_FROM_BETA_SECRET_STORE__"

	for _, appEnv := range []string{"", "development", "staging", "production"} {
		t.Setenv("APP_ENV", appEnv)
		t.Setenv("AI_CONFIG_KEK", placeholder)

		err := ValidateAIKeyEncryptionAtStartup()
		if err == nil {
			t.Fatalf("APP_ENV=%q: booted with a placeholder KEK. Secrets would be encrypted with a "+
				"string committed to this repository.", appEnv)
		}
		if !strings.Contains(err.Error(), "AI_CONFIG_KEK") {
			t.Errorf("APP_ENV=%q: the error must name the variable: %v", appEnv, err)
		}
		if !strings.Contains(err.Error(), "re-entered") && !strings.Contains(err.Error(), "re-enter") {
			t.Errorf("APP_ENV=%q: the error should warn that secrets stored under the placeholder "+
				"cannot be recovered and must be re-entered: %v", appEnv, err)
		}
	}
}

// A real KEK passes, and the dev fallback keeps its existing behaviour.
//
// The second half is a guard against overcorrection: it would be easy to make every weak-looking KEK
// fatal and break `make dev` and every local checkout, which is precisely what the dev fallback exists
// to avoid. Production still refuses it — that policy predates this change and is deliberately intact.
func TestValidateAIKeyEncryptionAcceptsRealKeyAndKeepsFallbackPolicy(t *testing.T) {
	t.Run("a real key passes", func(t *testing.T) {
		t.Setenv("APP_ENV", "production")
		t.Setenv("AI_CONFIG_KEK", "kZ8vQ2mXpL7wR4nT9yB3cF6hJ1dG5sA0eU8iO2kM4qW7zX1vN3bC5rY9tH6jP2lD")

		if err := ValidateAIKeyEncryptionAtStartup(); err != nil {
			t.Fatalf("a 64-character generated key was rejected: %v", err)
		}
	})

	t.Run("unset is tolerated outside production", func(t *testing.T) {
		t.Setenv("APP_ENV", "development")
		t.Setenv("AI_CONFIG_KEK", "")

		if err := ValidateAIKeyEncryptionAtStartup(); err != nil {
			t.Fatalf("local dev must still start with no KEK configured: %v", err)
		}
	})

	t.Run("unset still refuses in production", func(t *testing.T) {
		t.Setenv("APP_ENV", "production")
		t.Setenv("AI_CONFIG_KEK", "")

		if err := ValidateAIKeyEncryptionAtStartup(); err == nil {
			t.Fatal("production must not fall through to the public dev fallback key")
		}
	})
}
