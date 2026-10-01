//go:build integration

package integration_test

// The member-facing list of connected assistants joins grants, clients, tokens
// and agents. A renamed column would fail it only here, so it runs against the
// migrated schema: listing, ownership, and a revoked grant dropping out.

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	oauthModel "github.com/akashc777/OneCamp/models/postgres/OAuth"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestConnectionsForReadsTheRealSchema(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	db := postgresInit.DBConn.SqlDB

	me, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{me, other} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1,$2,$3,$4,NOW(),NOW())`, id, id.String()[:8]+"@example.test", "u"+id.String()[:6], "u"+id.String()[:6]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	clientID, err := oauthModel.CreateClient(ctx, "ChatGPT", []string{"https://chatgpt.com/cb"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	tokenID := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO api_tokens (id, name, token_hash, token_prefix, scopes, created_by)
		VALUES ($1,'ChatGPT',$2,'oc_abcd','["tasks:read","messages:write"]',$3)`, tokenID, "h-"+tokenID.String(), me); err != nil {
		t.Fatalf("token: %v", err)
	}
	grant := &oauthModel.Grant{ClientID: clientID, UserID: me, AgentID: uuid.New(), TokenID: tokenID, RefreshExpiresAt: time.Now().Add(time.Hour)}
	if err := oauthModel.CreateGrant(ctx, grant, "r-"+tokenID.String()); err != nil {
		t.Fatalf("grant: %v", err)
	}

	mine, err := oauthModel.ConnectionsFor(ctx, me)
	if err != nil {
		t.Fatalf("ConnectionsFor: %v", err)
	}
	if len(mine) != 1 || mine[0].ClientName != "ChatGPT" || len(mine[0].Scopes) != 2 {
		t.Fatalf("got %+v, want one ChatGPT connection with two scopes", mine)
	}
	if mine[0].AgentName != "" {
		t.Fatalf("an agent that does not exist must read as an empty name, got %q", mine[0].AgentName)
	}
	theirs, err := oauthModel.ConnectionsFor(ctx, other)
	if err != nil || len(theirs) != 0 {
		t.Fatalf("another person sees %d connections (err %v), want 0", len(theirs), err)
	}
	if owner, err := oauthModel.GrantOwner(ctx, mine[0].ID); err != nil || owner != me {
		t.Fatalf("GrantOwner = %v, %v; want %v", owner, err, me)
	}
	if _, err := oauthModel.RevokeGrant(ctx, mine[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if owner, _ := oauthModel.GrantOwner(ctx, mine[0].ID); owner != uuid.Nil {
		t.Fatal("a revoked grant must have no owner")
	}
	if after, _ := oauthModel.ConnectionsFor(ctx, me); len(after) != 0 {
		t.Fatalf("a revoked connection still listed: %+v", after)
	}
}
