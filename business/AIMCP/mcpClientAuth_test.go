package business

import (
	"errors"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
)

// A server whose stored secret will not decrypt must never yield a client.
//
// WHY. The model used to set HasAuthSecret = true and then log the decrypt failure and carry on,
// leaving AuthSecret empty. The registry went on to register that server's tools, and beta showed
// the two lines together: "decrypt auth secret failed for server 71289cdc..." immediately followed
// by "registered 44 tool(s) from 1 server(s)". Every call would then have left the process with an
// EMPTY auth header, to an external endpoint about to receive workspace data — refused by a
// correct remote for a reason that points at the wrong thing, and accepted by a permissive one,
// which means the authentication was never enforced.
//
// The failure is silent by construction: an empty header is a well-formed request. So it is pinned
// here rather than left to a reviewer noticing that a bool was set before the operation that
// justifies it.
func TestAServerWithAnUnreadableSecretYieldsNoClient(t *testing.T) {
	header := "Authorization"
	unusable := &model.McpServer{
		Name:                 "vendor-tools",
		URL:                  "https://mcp.example.com",
		AuthType:             "bearer",
		AuthHeaderName:       &header,
		AuthSecretUnreadable: true,
	}

	client, err := NewClient(unusable)
	if err == nil {
		t.Fatal("NewClient returned no error for a server whose auth secret cannot be decrypted; " +
			"the next call would go out with an empty auth header")
	}
	if client != nil {
		t.Error("NewClient returned a usable client alongside the error; a caller ignoring the " +
			"error would still make the unauthenticated call")
	}
	// Named so a caller can tell this apart from a transport failure: nothing about the remote is
	// wrong and retrying cannot help until an admin re-enters the secret.
	if !errors.Is(err, ErrAuthSecretUnreadable) {
		t.Errorf("error is %v, want ErrAuthSecretUnreadable so callers do not report it as a "+
			"connection problem", err)
	}
}

// The ordinary case still works, so the guard above cannot be satisfied by refusing everything.
func TestAServerWithAReadableSecretStillYieldsAClient(t *testing.T) {
	header := "Authorization"
	usable := &model.McpServer{
		Name:           "vendor-tools",
		URL:            "https://mcp.example.com",
		AuthType:       "bearer",
		AuthHeaderName: &header,
		AuthSecret:     "decrypted-secret",
		HasAuthSecret:  true,
	}

	client, err := NewClient(usable)
	if err != nil {
		t.Fatalf("NewClient refused a server with a readable secret: %v", err)
	}
	if client == nil {
		t.Fatal("NewClient returned no client and no error")
	}
	if client.authSecret != "decrypted-secret" {
		t.Errorf("client carries authSecret %q, want the decrypted secret — an empty one would be "+
			"sent as an empty header", client.authSecret)
	}
}
