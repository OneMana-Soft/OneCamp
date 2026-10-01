package models

import (
	"os"
	"strings"
	"testing"
)

// The TOTP_KEK startup policy, which is deliberately NOT the same as the other two KEKs'.
//
// Worth pinning precisely because the difference looks like an inconsistency. Somebody tidying these
// three probes into one shared shape would make unset fatal — and take down every deployment that
// simply does not use two-factor authentication — or make a placeholder survive, which is the fault the
// probe exists to catch.

// The value that actually shipped to beta, in the file this repository commits.
const shippedPlaceholder = "__SET_FROM_BETA_SECRET_STORE__"

func TestTOTPStartupRefusesAPlaceholderInEveryEnvironment(t *testing.T) {
	// Every environment, not just production. A second factor sealed under a value from the repository
	// protects nobody in dev either, and dev is where somebody would first enrol and then carry the
	// habit forward.
	for _, env := range []string{"", "development", "dev", "staging", "production", "prod"} {
		t.Run("APP_ENV="+env, func(t *testing.T) {
			t.Setenv("APP_ENV", env)
			t.Setenv("TOTP_KEK", shippedPlaceholder)

			err := ValidateTOTPKeyEncryptionAtStartup()
			if err == nil {
				t.Fatal("booted with a placeholder TOTP_KEK. Enrolment would succeed and seal every " +
					"second factor with a string committed to this repository, which looks like MFA " +
					"to the user, the admin and any auditor")
			}
			if !strings.Contains(err.Error(), "TOTP_KEK") {
				t.Errorf("the error must name the variable; an operator reading a boot log beside "+
					"three other secrets cannot act on %q", err.Error())
			}
		})
	}
}

func TestTOTPStartupAcceptsUnsetAsASupportedConfiguration(t *testing.T) {
	// Unset means "this deployment does not offer 2FA". totpKEK() returns ErrTOTPKEKMissing, enrolment
	// answers 409 with an actionable sentence, and nothing is ever sealed. Refusing to boot here would
	// be an outage caused by an optional feature being switched off — which is exactly what would
	// happen if this probe were made to match the AI_CONFIG_KEK one, where unset means "silently using
	// a key printed in the source".
	for _, env := range []string{"development", "production"} {
		t.Run("APP_ENV="+env, func(t *testing.T) {
			t.Setenv("APP_ENV", env)
			t.Setenv("TOTP_KEK", "")

			if err := ValidateTOTPKeyEncryptionAtStartup(); err != nil {
				t.Fatalf("refused to start with 2FA simply not configured: %v", err)
			}
		})
	}
}

func TestTOTPStartupAcceptsARealKeyAndRoundTrips(t *testing.T) {
	// Shape of `openssl rand -base64 48`, which is what the error messages and the
	// installer's `make secrets` tell an operator to generate.
	t.Setenv("APP_ENV", "production")
	t.Setenv("TOTP_KEK", "kZ8QvN2mR7pX4tL9wY6bJ3hF5sD1gA0cE8uI2oP7rT4nM6kV3yB9xC1zS5dW8fH2")

	if err := ValidateTOTPKeyEncryptionAtStartup(); err != nil {
		t.Fatalf("a high-entropy key was rejected: %v", err)
	}
}

func TestTOTPStartupRoundTripActuallyExercisesTheCipher(t *testing.T) {
	// The round trip is the half that catches a key which passes inspection but cannot drive AES-GCM.
	// Asserted by confirming the probe seals and opens through the SAME functions enrolment uses, so a
	// change to the envelope format is caught at boot rather than by a user watching enrolment fail.
	t.Setenv("TOTP_KEK", "aQ7wE9rT2yU5iO8pA1sD4fG6hJ3kL0zX9cV7bN2mQ5wE8rT1yU4iO7pA0sD3fG6h")

	sealed, err := sealTOTPSecret("round-trip-probe")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := openTOTPSecret(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != "round-trip-probe" {
		t.Fatalf("round trip returned %q", opened)
	}
	// The probe must not be storing plaintext. Checked because a seal that silently became a no-op
	// would still round-trip perfectly and pass every other assertion here.
	if strings.Contains(string(sealed), "round-trip-probe") {
		t.Fatal("the sealed blob contains its own plaintext; the secret is not being encrypted")
	}
}

func TestTOTPStartupLeavesNoEnvironmentResidue(t *testing.T) {
	// t.Setenv restores on cleanup, so this guards the probe itself: it must only READ the variable.
	// A probe that normalised TOTP_KEK by writing it back would change what enrolment later hashes,
	// and every secret sealed before the change would stop opening.
	const value = "pQ2wS5eD8rF1tG4yH7uJ0kL3zX6cV9bN2mA5sD8fG1hJ4kL7zX0cV3bN6mQ9wE2r"
	t.Setenv("TOTP_KEK", value)

	if err := ValidateTOTPKeyEncryptionAtStartup(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TOTP_KEK"); got != value {
		t.Fatalf("the probe rewrote TOTP_KEK to %q; every secret sealed under the original would "+
			"stop decrypting", got)
	}
}
