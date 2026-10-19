package helpers

import (
	"bytes"
	"encoding/base64"
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
