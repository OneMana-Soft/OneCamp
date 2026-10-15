package business

import (
	"context"
	"net/http"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
	"github.com/google/uuid"
)

// What an MCP server may reach is decided as it connects, by whether whoever
// added it is an admin now: an admin's may be on this server's own network, a
// member's public addresses only (helpers.EndpointPolicyFor).
func TestAServersReachFollowsWhoAddedIt(t *testing.T) {
	admin, member := uuid.New(), uuid.New()
	oldAdmin, oldClient := creatorIsAdmin, newMCPHTTPClient
	t.Cleanup(func() { creatorIsAdmin, newMCPHTTPClient = oldAdmin, oldClient })
	creatorIsAdmin = func(_ context.Context, id *uuid.UUID) bool { return id != nil && *id == admin }
	var got helpers.EndpointPolicy
	newMCPHTTPClient = func(p helpers.EndpointPolicy) *http.Client { got = p; return http.DefaultClient }

	for _, c := range []struct {
		by      *uuid.UUID
		private bool
	}{{&admin, true}, {&member, false}, {nil, false}} {
		if _, err := NewClient(context.Background(), &model.McpServer{URL: "https://mcp.example.com", AuthType: model.AuthNone, CreatedBy: c.by}); err != nil {
			t.Fatal(err)
		}
		if got.AddedByAdmin != c.private {
			t.Errorf("created by %v: policy %+v; want added by an admin = %v", c.by, got, c.private)
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
