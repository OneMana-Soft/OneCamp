package models

import (
	"os"
	"strings"
	"testing"
)

// TestEncryptDecryptRoundTrip confirms the AES-256-GCM round-trip
// produces the original plaintext. Uses the package-level encryptToken /
// decryptToken so this exercises the same path startups do.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Setenv("IMPORT_TOKEN_KEK", "test-key-with-enough-entropy-for-tests-1234")

	plaintexts := []string{
		"short",
		"one-medium-token-50-chars-or-so-aaaaaaaaaaaaaaaa",
		strings.Repeat("x", 4096), // big payload to confirm streaming
		"unicode-世界-emoji-🚀",
	}
	for _, p := range plaintexts {
		ct, err := encryptToken(p)
		if err != nil {
			t.Fatalf("encryptToken: %v", err)
		}
		// Each ciphertext must include nonce(12) + tag(16) + plaintext bytes.
		if len(ct) < 12+16 {
			t.Fatalf("ciphertext too short: %d", len(ct))
		}
		// Two encryptions of the same plaintext must differ (random nonce).
		ct2, err := encryptToken(p)
		if err != nil {
			t.Fatalf("encryptToken second: %v", err)
		}
		if string(ct) == string(ct2) {
			t.Fatalf("encryptToken produced identical ciphertexts; nonce reused?")
		}
		got, err := decryptToken(ct)
		if err != nil {
			t.Fatalf("decryptToken: %v", err)
		}
		if got != p {
			t.Fatalf("round-trip mismatch: got %q want %q", got, p)
		}
	}
}

func TestDecryptToken_TamperDetected(t *testing.T) {
	t.Setenv("IMPORT_TOKEN_KEK", "test-key-tamper-detect-aaaaaaaaaaaaaaaaaaaaa")
	ct, err := encryptToken("hello")
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the ciphertext body. GCM's tag must reject.
	ct[len(ct)-1] ^= 0x01
	if _, err := decryptToken(ct); err == nil {
		t.Fatal("expected GCM tag failure for tampered ciphertext")
	}
}

func TestDecryptToken_EmptyRejected(t *testing.T) {
	if _, err := decryptToken(nil); err == nil {
		t.Fatal("expected error for nil ciphertext")
	}
	if _, err := decryptToken([]byte{0x00, 0x01, 0x02}); err == nil {
		t.Fatal("expected error for too-short ciphertext")
	}
}

func TestEncryptToken_EmptyRejected(t *testing.T) {
	t.Setenv("IMPORT_TOKEN_KEK", "test-empty-reject-aaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := encryptToken(""); err == nil {
		t.Fatal("expected error for empty plaintext")
	}
}

func TestDifferentKEKsCannotDecrypt(t *testing.T) {
	t.Setenv("IMPORT_TOKEN_KEK", "kek-one-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	ct, err := encryptToken("secret-token")
	if err != nil {
		t.Fatal(err)
	}
	// Switch the KEK and try to decrypt the original ciphertext. GCM
	// must refuse because the derived key is different.
	t.Setenv("IMPORT_TOKEN_KEK", "kek-two-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := decryptToken(ct); err == nil {
		t.Fatal("expected decrypt to fail under a different KEK")
	}
}

func TestValidateTokenEncryptionAtStartup_DevFallbackInProd(t *testing.T) {
	// Prod env + unset KEK → must fail to start.
	t.Setenv("APP_ENV", "production")
	if err := os.Unsetenv("IMPORT_TOKEN_KEK"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTokenEncryptionAtStartup(); err == nil {
		t.Fatal("expected failure when prod has no KEK")
	}

	// Prod env + dev fallback string → must fail.
	t.Setenv("IMPORT_TOKEN_KEK", devFallbackKEK)
	if err := ValidateTokenEncryptionAtStartup(); err == nil {
		t.Fatal("expected failure when prod uses the dev fallback")
	}

	// Prod env + real KEK → ok.
	t.Setenv("IMPORT_TOKEN_KEK", "real-prod-kek-with-enough-entropy-1234567890")
	if err := ValidateTokenEncryptionAtStartup(); err != nil {
		t.Fatalf("expected success with a real KEK in prod, got %v", err)
	}
}

func TestValidateTokenEncryptionAtStartup_DevToleratesFallback(t *testing.T) {
	// Non-prod env: dev fallback is allowed (with a warning).
	t.Setenv("APP_ENV", "development")
	t.Setenv("IMPORT_TOKEN_KEK", devFallbackKEK)
	if err := ValidateTokenEncryptionAtStartup(); err != nil {
		t.Fatalf("dev should tolerate fallback, got %v", err)
	}
	// Non-prod env + unset is also fine.
	if err := os.Unsetenv("IMPORT_TOKEN_KEK"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTokenEncryptionAtStartup(); err != nil {
		t.Fatalf("dev should tolerate unset KEK, got %v", err)
	}
}

// The import-token probe must refuse a placeholder KEK, in every environment.
//
// Mirrors models/postgres/AI TestValidateAIKeyEncryptionRefusesPlaceholder, because both KEKs shipped
// to beta with the SAME unsubstituted value out of vars/.env.beta and both probes accepted it: each
// asked only "empty?" and "dev fallback?", and __SET_FROM_BETA_SECRET_STORE__ is neither. OAuth import
// tokens were therefore encrypted with a string committed to this repository.
//
// APP_ENV is exercised unset as well as production because unset is how beta actually runs — the
// variable is in no env template and no compose file, so the production-only branches below could
// never fire there.
func TestValidateTokenEncryptionRefusesPlaceholder(t *testing.T) {
	const placeholder = "__SET_FROM_BETA_SECRET_STORE__"

	for _, appEnv := range []string{"", "development", "staging", "production"} {
		t.Setenv("APP_ENV", appEnv)
		t.Setenv("IMPORT_TOKEN_KEK", placeholder)

		err := ValidateTokenEncryptionAtStartup()
		if err == nil {
			t.Fatalf("APP_ENV=%q: booted with a placeholder KEK; OAuth tokens would be encrypted "+
				"with a value committed to this repository", appEnv)
		}
		if !strings.Contains(err.Error(), "IMPORT_TOKEN_KEK") {
			t.Errorf("APP_ENV=%q: the error must name the variable: %v", appEnv, err)
		}
	}
}
