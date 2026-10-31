package helpers

import (
	"bytes"
	"testing"
)

// A purpose's key is the same every time and another purpose's is another;
// neither is the server's secret, and no key is made without one.
func TestAPurposeKeyIsItsPurposesAlone(t *testing.T) {
	t.Setenv("JWT_SECRET", "a server's secret")
	cards, err := PurposeKey("onecamp.cards.v1")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := PurposeKey("onecamp.cards.v1")
	anonymous, _ := PurposeKey("onecamp.anonymous.v1")
	if !bytes.Equal(cards, again) || bytes.Equal(cards, anonymous) || len(cards) != 32 {
		t.Errorf("keys: %x %x %x", cards, again, anonymous)
	}
	if bytes.Contains(cards, []byte("a server's secret")) {
		t.Error("the key carries the secret")
	}
	t.Setenv("JWT_SECRET", "")
	if _, err := PurposeKey("onecamp.cards.v1"); err == nil {
		t.Error("a key made with no secret")
	}
}
