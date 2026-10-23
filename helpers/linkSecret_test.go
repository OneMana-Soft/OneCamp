package helpers

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestALinkSecretIsUnguessableAndKeptOnlyAsItsHash(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		raw, hash, err := NewLinkSecret()
		if err != nil {
			t.Fatal(err)
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(b) != 32 {
			t.Fatalf("%q is not 32 bytes, URL-safe: %v", raw, err)
		}
		if seen[raw] {
			t.Fatal("the same secret twice")
		}
		seen[raw] = true
		if !bytes.Equal(hash, LinkSecretHash(raw)) || len(hash) != 32 || bytes.Contains(hash, []byte(raw)) {
			t.Fatal("the hash is the secret's SHA-256, and isn't the secret")
		}
	}
}

func TestASealedLinkSecretOpensOnlyAsTheSecretItsHashFinds(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "the server's key")
	raw, hash, sealed, err := NewSealedLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash, LinkSecretHash(raw)) || sealed == "" || strings.Contains(sealed, raw) {
		t.Fatalf("hash %x, sealed %q: the hash finds the secret, and the seal doesn't show it", hash, sealed)
	}
	if got := OpenLinkSecret(sealed, hash); got != raw {
		t.Errorf("opened %q, want %q", got, raw)
	}
	_, _, another, err := NewSealedLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	if got := OpenLinkSecret(another, hash); got != "" {
		t.Errorf("another link's seal opened as this one's: %q", got)
	}
	if got := OpenLinkSecret("", hash); got != "" {
		t.Errorf("no seal opened as %q", got)
	}
	t.Setenv("APP_SECRET_KEK", "the server's key, changed since")
	if got := OpenLinkSecret(sealed, hash); got != "" {
		t.Errorf("a seal opened under another key: %q", got)
	}
}
