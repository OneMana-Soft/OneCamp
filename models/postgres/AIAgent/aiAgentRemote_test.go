package models

import (
	"testing"

	"github.com/google/uuid"
)

// A stored secret reads back; ciphertext that cannot be read reads as none,
// with unreadable saying why, so the agent never goes out with an empty
// credential to an endpoint that expects one.
func TestTheRemoteSecretReadsBackOrReadsAsUnreadable(t *testing.T) {
	enc, err := encryptAGUISecret("tok")
	if err != nil {
		t.Fatal(err)
	}
	if secret, set, unreadable := readAGUISecret(uuid.New(), enc); secret != "tok" || !set || unreadable {
		t.Errorf("round trip = %q %v %v", secret, set, unreadable)
	}
	if secret, set, unreadable := readAGUISecret(uuid.New(), nil); secret != "" || set || unreadable {
		t.Errorf("none stored = %q %v %v", secret, set, unreadable)
	}
	if secret, set, unreadable := readAGUISecret(uuid.New(), []byte("not ciphertext")); secret != "" || set || !unreadable {
		t.Errorf("garbage = %q %v %v, want unset and unreadable", secret, set, unreadable)
	}
	if enc, err := encryptAGUISecret("  "); err != nil || enc != nil {
		t.Errorf("a blank secret must store as NULL, got %v %v", enc, err)
	}
}

func TestRemoteIsTheEndpointBeingSet(t *testing.T) {
	if (&AiAgent{}).Remote() || (&AiAgent{AGUIEndpoint: " "}).Remote() {
		t.Error("no endpoint must not be remote")
	}
	if !(&AiAgent{AGUIEndpoint: "https://r"}).Remote() {
		t.Error("an endpoint must be remote")
	}
	var nilAgent *AiAgent
	if nilAgent.Remote() {
		t.Error("nil must not be remote")
	}
}
