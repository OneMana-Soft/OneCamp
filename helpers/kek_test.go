package helpers

import (
	"strings"
	"testing"
)

// The value that actually shipped must be caught.
//
// This is the regression test, and it is named for the real incident rather than for a category. Beta
// ran for months with AI_CONFIG_KEK and IMPORT_TOKEN_KEK both set to this string, copied out of
// vars/.env.beta and never substituted. Every provider API key, MCP auth secret and OAuth import
// token in that database was encrypted with SHA-256 of a value committed to the repository.
//
// Both startup probes accepted it, because both asked only "is it empty?" and "is it the dev
// fallback?" — and it is neither. It surfaced by accident, as an undecryptable key after the value
// later changed. A weak key does not announce itself; it works perfectly.
func TestInspectKEKCatchesTheValueThatShipped(t *testing.T) {
	for _, name := range []string{"AI_CONFIG_KEK", "IMPORT_TOKEN_KEK"} {
		status := InspectKEK(name, "__SET_FROM_BETA_SECRET_STORE__", "dev-fallback-value")

		if !status.Placeholder {
			t.Errorf("%s: the placeholder that shipped to beta was not recognised", name)
		}
		if !status.Unusable() {
			t.Errorf("%s: a placeholder must stop startup in every environment, not only production", name)
		}
		if !strings.Contains(status.Reason, name) {
			t.Errorf("%s: the reason must name the variable, or a log line beside three other "+
				"secrets is useless: %q", name, status.Reason)
		}
	}
}

// Placeholder shapes, and the values that must NOT be mistaken for one.
//
// The negative half matters more than the positive half. A false positive here refuses to boot, so a
// generated secret that happens to contain a marker substring would be a self-inflicted outage. That
// is why the marker list has no four-character entries like "todo" or "xxxx".
func TestInspectKEKPlaceholderDetection(t *testing.T) {
	placeholders := []string{
		"__SET_FROM_BETA_SECRET_STORE__",
		"__SET_FROM_SECRET_STORE__",
		"__CHANGEME__",
		"changeme",
		"CHANGE_ME_IN_PRODUCTION",
		"replace-me-with-a-real-key",
		"placeholder-kek-value-here",
		"<your-kek-here>",
		"set from secret_store",
		"fill_me_in",
		"example-kek-do-not-use",
	}
	for _, value := range placeholders {
		if status := InspectKEK("AI_CONFIG_KEK", value, ""); !status.Placeholder {
			t.Errorf("%q should be recognised as a template placeholder", value)
		}
	}

	// Real secrets, including generated output of the kind `make secrets` produces.
	realKeys := []string{
		"kZ8vQ2mXpL7wR4nT9yB3cF6hJ1dG5sA0eU8iO2kM4qW7zX1vN3bC5rY9tH6jP2lD",
		"aGVsbG8td29ybGQtdGhpcy1pcy1hLXJlYWwtYmFzZTY0LXNlY3JldC12YWx1ZQ==",
		"f4c2a9e17b8d3506af21ce9b47d8e0f3125a6c8b9d4e7f0a1b2c3d4e5f6a7b8c",
		"make-test-ai-kek-with-enough-entropy-1234567890",
		"correct-horse-battery-staple-and-then-some-more-words-for-length",
	}
	for _, value := range realKeys {
		status := InspectKEK("AI_CONFIG_KEK", value, "")
		if status.Placeholder {
			t.Errorf("%q was wrongly flagged as a placeholder. A false positive here refuses to "+
				"start a healthy deployment: %s", value, status.Reason)
		}
		if status.Reason != "" {
			t.Errorf("%q should be accepted without comment, got: %s", value, status.Reason)
		}
	}
}

// Unset and dev-fallback stay non-fatal, which is the pre-existing policy and deliberately unchanged.
//
// The distinction from a placeholder is the whole design. The dev fallback is a documented
// convenience so a local checkout starts without ceremony, so tolerating it outside production is a
// real trade someone made on purpose. Nobody ever meant to type __SET_FROM_BETA_SECRET_STORE__.
func TestInspectKEKFallbackIsNotFatalEverywhere(t *testing.T) {
	const fallback = "onecamp-dev-ai-config-kek-please-override-in-production"

	for _, value := range []string{"", fallback} {
		status := InspectKEK("AI_CONFIG_KEK", value, fallback)

		if !status.Fallback {
			t.Errorf("%q should be reported as the fallback", value)
		}
		if status.Unusable() {
			t.Errorf("%q must stay non-fatal outside production; making it fatal everywhere would "+
				"break a local checkout, which is what the fallback exists for", value)
		}
		if status.Reason == "" {
			t.Errorf("%q should still explain itself so the startup warning has something to say", value)
		}
	}
}

// A short but real-looking key is advisory, never fatal.
//
// Refusing to boot over length would be a judgement call enforced as an outage: an operator may have
// a shorter genuinely-random value in a live deployment. A placeholder is not a judgement call, which
// is why only that one is fatal.
func TestInspectKEKWeakIsAdvisoryOnly(t *testing.T) {
	status := InspectKEK("AI_CONFIG_KEK", "short-key-123", "")

	if !status.Weak {
		t.Fatalf("a 13-character key should be reported as weak, got %+v", status)
	}
	if status.Unusable() {
		t.Error("length alone must not stop startup")
	}
	if status.Placeholder || status.Fallback {
		t.Errorf("a short real key is neither a placeholder nor the fallback: %+v", status)
	}
	if !strings.Contains(status.Reason, "13") {
		t.Errorf("the warning should state the actual length so it is actionable: %q", status.Reason)
	}
}

// Whitespace must not smuggle a placeholder past the check.
//
// Env files acquire trailing spaces, and an unquoted value pasted with a newline is common. Comparing
// untrimmed would mean " __SET_FROM_BETA_SECRET_STORE__" passes as a real key — the exact bug, with
// one extra character.
func TestInspectKEKTrimsBeforeDeciding(t *testing.T) {
	if status := InspectKEK("AI_CONFIG_KEK", "  __SET_FROM_BETA_SECRET_STORE__\t", ""); !status.Placeholder {
		t.Error("a padded placeholder must still be recognised")
	}
	if status := InspectKEK("AI_CONFIG_KEK", "   ", ""); !status.Fallback {
		t.Error("whitespace-only should be treated as unset, not as a real key")
	}
}

// A bare "__" must not be read as a placeholder.
//
// Pinned because the wrapper rule is prefix-and-suffix "__", which "__" satisfies twice over with no
// content between. It is a nonsense KEK either way, but it should be reported as weak rather than as
// a template, so the operator is told what is actually wrong with it.
func TestInspectKEKDoesNotOverreachOnUnderscores(t *testing.T) {
	status := InspectKEK("AI_CONFIG_KEK", "__", "")

	if status.Placeholder {
		t.Error(`"__" is not a template placeholder; it has no template in it`)
	}
	if !status.Weak {
		t.Errorf(`"__" should be reported as weak, got %+v`, status)
	}
}
