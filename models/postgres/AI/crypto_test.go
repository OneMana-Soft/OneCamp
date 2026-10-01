package models

import "testing"

// TestEncryptDecryptAPIKeyRoundTrip confirms the AES-256-GCM round-trip
// returns the original plaintext and that two encryptions differ (random
// nonce), mirroring the import-token KEK test.
func TestEncryptDecryptAPIKeyRoundTrip(t *testing.T) {
	t.Setenv("AI_CONFIG_KEK", "test-ai-kek-with-enough-entropy-123456")

	keys := []string{
		"sk-proj-abc123",
		"sk-ant-xyz789",
		"a", // short
		"key with spaces and = signs and / slashes",
	}
	for _, k := range keys {
		ct, err := EncryptAPIKey(k)
		if err != nil {
			t.Fatalf("EncryptAPIKey(%q): %v", k, err)
		}
		if len(ct) < 12+16 {
			t.Fatalf("ciphertext too short for %q: %d bytes", k, len(ct))
		}

		ct2, err := EncryptAPIKey(k)
		if err != nil {
			t.Fatalf("EncryptAPIKey second: %v", err)
		}
		if string(ct) == string(ct2) {
			t.Fatalf("identical ciphertexts for %q — nonce reuse?", k)
		}

		got, err := DecryptAPIKey(ct)
		if err != nil {
			t.Fatalf("DecryptAPIKey: %v", err)
		}
		if got != k {
			t.Fatalf("round-trip mismatch: got %q want %q", got, k)
		}
	}
}

func TestEncryptAPIKey_EmptyRejected(t *testing.T) {
	t.Setenv("AI_CONFIG_KEK", "test-ai-kek-with-enough-entropy-123456")
	if _, err := EncryptAPIKey(""); err == nil {
		t.Fatal("expected error encrypting empty string")
	}
}

func TestDecryptAPIKey_TamperDetected(t *testing.T) {
	t.Setenv("AI_CONFIG_KEK", "test-ai-kek-tamper-detect-aaaaaaaaaaaa")
	ct, err := EncryptAPIKey("hello-secret")
	if err != nil {
		t.Fatal(err)
	}
	ct[len(ct)-1] ^= 0x01 // flip a tag byte
	if _, err := DecryptAPIKey(ct); err == nil {
		t.Fatal("expected GCM tag failure for tampered ciphertext")
	}
}

// TestIsUsingDevFallbackKEK is gone with the function it covered. It asserted that an empty
// AI_CONFIG_KEK means "fallback in use", which was true and insufficient: the value that actually
// shipped to beta was non-empty, so the test passed while the deployment encrypted every provider key
// with a string from this repository. helpers.InspectKEK now answers that question, and
// helpers/kek_test.go covers it including the placeholder case this one could not express.
