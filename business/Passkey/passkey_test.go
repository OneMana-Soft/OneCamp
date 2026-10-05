package business

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	passkeyModel "github.com/akashc777/OneCamp/models/postgres/Passkey"
)

func TestRelyingParty(t *testing.T) {
	id, origins, err := RelyingParty("https://onecamp.acme.com/")
	if err != nil || id != "onecamp.acme.com" || !reflect.DeepEqual(origins, []string{"https://onecamp.acme.com"}) {
		t.Fatalf("got %q %v %v", id, origins, err)
	}
	id, origins, err = RelyingParty("localhost:3000")
	if err != nil || id != "localhost" || len(origins) < 2 {
		t.Fatalf("localhost: got %q %v %v", id, origins, err)
	}
	for _, bad := range []string{"", "  ", "a b", "host/path"} {
		if _, _, err := RelyingParty(bad); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%q: want not configured, got %v", bad, err)
		}
	}
}

func TestPasskeyName(t *testing.T) {
	if n, err := PasskeyName("  My   MacBook "); err != nil || n != "My MacBook" {
		t.Fatalf("got %q %v", n, err)
	}
	if n, _ := PasskeyName(""); n != "Passkey" {
		t.Errorf("an empty name should get a default, got %q", n)
	}
	if _, err := PasskeyName(strings.Repeat("x", 61)); err == nil {
		t.Error("a 61-character name was accepted")
	}
}

// A passkey never outranks the identity provider, and opens nothing for
// accounts that can't sign in.
func TestCanSignIn(t *testing.T) {
	if CanSignIn(&passkeyModel.Owner{Email: "maya@acme.com"}) != nil {
		t.Error("a member was refused")
	}
	for name, o := range map[string]*passkeyModel.Owner{
		"deleted":     nil,
		"external":    {IsExternal: true},
		"sso managed": {IsSSOManaged: true},
	} {
		if CanSignIn(o) == nil {
			t.Errorf("%s: signed in with a passkey", name)
		}
	}
}
