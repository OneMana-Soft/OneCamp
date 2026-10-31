package helpers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
)

// The same person in the same scope gets the same key; another scope or
// person, another key; and no key is made without the server's secret.
func TestAnAnonymousKeyIsStableAndScoped(t *testing.T) {
	t.Setenv("JWT_SECRET", "a server's secret")
	a, err := AnonymousKey("poll:1", "0xben")
	if err != nil {
		t.Fatal(err)
	}
	same, _ := AnonymousKey("poll:1", "0xben")
	otherScope, _ := AnonymousKey("poll:2", "0xben")
	otherPerson, _ := AnonymousKey("poll:1", "0xmaya")
	// The scope and the person can't run into each other.
	joined, _ := AnonymousKey("poll:10", "xben")
	if !bytes.Equal(a, same) || bytes.Equal(a, otherScope) || bytes.Equal(a, otherPerson) || bytes.Equal(a, joined) {
		t.Errorf("keys: %x %x %x %x %x", a, same, otherScope, otherPerson, joined)
	}
	if bytes.Contains(a, []byte("0xben")) {
		t.Error("the key carries who it stands for")
	}
	// The server's secret signs sessions too, so the key is made under one
	// derived for this purpose, never under the secret itself.
	bare := hmac.New(sha256.New, []byte("a server's secret"))
	bare.Write([]byte("poll:1\x000xben"))
	if bytes.Equal(a, bare.Sum(nil)) {
		t.Error("the key is made under the server's secret itself")
	}
	t.Setenv("JWT_SECRET", "")
	if _, err := AnonymousKey("poll:1", "0xben"); err == nil {
		t.Error("a key made with no secret")
	}
}
