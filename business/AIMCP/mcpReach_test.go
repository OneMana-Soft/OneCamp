package business

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
	"github.com/google/uuid"
)

// What an MCP server may reach is decided as it connects, by whether whoever
// added it is an admin now: an admin's may be on this server's own network, a
// member's public addresses only (safefetch.EndpointPolicy).
func TestAServersReachFollowsWhoAddedIt(t *testing.T) {
	admin, member := uuid.New(), uuid.New()
	oldAdmin, oldClient := creatorIsAdmin, newMCPHTTPClient
	t.Cleanup(func() { creatorIsAdmin, newMCPHTTPClient = oldAdmin, oldClient })
	creatorIsAdmin = func(_ context.Context, id *uuid.UUID) bool { return id != nil && *id == admin }
	var got safefetch.Policy
	newMCPHTTPClient = func(p safefetch.Policy) *http.Client { got = p; return http.DefaultClient }

	for _, c := range []struct {
		by      *uuid.UUID
		private bool
	}{{&admin, true}, {&member, false}, {nil, false}} {
		if _, err := NewClient(context.Background(), &model.McpServer{URL: "https://mcp.example.com", AuthType: model.AuthNone, CreatedBy: c.by}); err != nil {
			t.Fatal(err)
		}
		if got.PrivateNetwork != c.private || !got.AnyPort {
			t.Errorf("created by %v: policy %+v; want private=%v on any port", c.by, got, c.private)
		}
	}
	if err := validate(&ServerInput{Name: "x", URL: "http://10.0.0.5:8082/mcp"}, false); err == nil {
		t.Error("a member's server on this server's network was accepted")
	}
	if err := validate(&ServerInput{Name: "x", URL: "http://10.0.0.5:8082/mcp"}, true); err != nil {
		t.Errorf("an admin's server on this server's network: %v", err)
	}
	for _, admin := range []bool{false, true} {
		if err := validate(&ServerInput{Name: "x", URL: "http://169.254.169.254/latest/meta-data/"}, admin); err == nil {
			t.Errorf("the metadata address was accepted (admin %v)", admin)
		}
	}
}

// A header name left on a bearer server from an earlier setting isn't where
// its token goes, so a rename doesn't ask for the token again.
func TestAStaleHeaderNameIsNotADestination(t *testing.T) {
	stale := "X-Old-Key"
	saved := &model.McpServer{URL: "https://mcp.example.com/mcp", AuthType: model.AuthBearer, AuthHeaderName: &stale}
	same := &model.McpServer{URL: "https://mcp.example.com/mcp", AuthType: model.AuthBearer}
	if helpers.SecretMoves(saved.SecretDestination(), same.SecretDestination()) {
		t.Error("a bearer server with a stale header name asked for its token again")
	}
	header := "X-Api-Key"
	headerAuth := &model.McpServer{URL: "https://mcp.example.com/mcp", AuthType: model.AuthHeader, AuthHeaderName: &header}
	if !helpers.SecretMoves(same.SecretDestination(), headerAuth.SecretDestination()) {
		t.Error("moving the token into a header must ask for it again")
	}
}

// A name only this server's own network resolves (a Docker service's, any
// single word) is refused when a member saves a server with it, as an address
// on that network is, rather than only when it connects. An admin, who runs
// such services beside OneCamp, may save it.
func TestASingleWordHostIsCheckedWhenTheServerIsSaved(t *testing.T) {
	for _, url := range []string{"http://go-service:8080/mcp", "http://postgres/"} {
		if err := validate(&ServerInput{Name: "Inside", URL: url}, false); err == nil || !strings.Contains(err.Error(), "Only an admin") {
			t.Errorf("a member saving %s: %v; want it refused", url, err)
		}
		if err := validate(&ServerInput{Name: "Inside", URL: url}, true); err != nil {
			t.Errorf("an admin saving %s: %v", url, err)
		}
	}
}
