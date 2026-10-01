package helpers

import "testing"

func TestEncryptSecretRoundTrip(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "test-kek-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	cases := []string{"hunter2", "a-very-long-giphy-api-key-1234567890", "ünïcödé secret 🔐"}
	for _, plain := range cases {
		enc, err := EncryptSecret(plain)
		if err != nil {
			t.Fatalf("EncryptSecret(%q): %v", plain, err)
		}
		if enc == plain {
			t.Fatalf("ciphertext equals plaintext for %q", plain)
		}
		got, err := DecryptSecret(enc)
		if err != nil {
			t.Fatalf("DecryptSecret: %v", err)
		}
		if got != plain {
			t.Errorf("round-trip mismatch: got %q want %q", got, plain)
		}
	}
}

func TestEncryptSecretEmpty(t *testing.T) {
	enc, err := EncryptSecret("")
	if err != nil || enc != "" {
		t.Fatalf("empty plaintext should yield empty ciphertext, got %q err %v", enc, err)
	}
	got, err := DecryptSecret("")
	if err != nil || got != "" {
		t.Fatalf("empty ciphertext should yield empty plaintext, got %q err %v", got, err)
	}
}

func TestDecryptSecretTampered(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "test-kek-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	enc, err := EncryptSecret("topsecret")
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the last base64 char so GCM auth fails.
	tampered := enc[:len(enc)-2] + "AA"
	if _, err := DecryptSecret(tampered); err == nil {
		t.Error("expected GCM auth failure on tampered ciphertext")
	}
}
