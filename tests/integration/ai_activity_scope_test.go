//go:build integration
// +build integration

package integration_test

// Who can see which decisions.
//
// WHAT THIS PROVES. The audit read behind the AI activity feed used to be
// admin-only, so a member got agent runs and no decisions at all: the person an
// agent acts FOR could not see that it had been stopped on their behalf, and had
// to take the product's central guarantee on trust.
//
// Opening it up is the kind of change that goes wrong quietly in the other
// direction — a member seeing the whole workspace's audit log would be a
// privilege escalation that no test would notice, because the feature would look
// like it was working. So both directions are asserted: a member sees their own
// refusal, and does NOT see somebody else's.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAIActivityScope -v

import (
	"context"
	"testing"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// person creates a user and records one refusal against them.
func person(t *testing.T, ctx context.Context, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email := name + "-" + id.String()[:8] + "@example.test"
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1,$2,$3,$4,NOW(),NOW())`, id, email, name, name); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	if err := auditBusiness.RecordForPrincipal(ctx, &id, email, auditBusiness.ActorAgent,
		"mcp.tool_call.refused", auditBusiness.CategoryAgent,
		"refused: "+name+" is not a member of that channel",
		map[string]interface{}{"tool": "send_message"}); err != nil {
		t.Fatalf("record refusal for %s: %v", name, err)
	}
	return id
}

func TestAIActivityScopeShowsAMemberTheirOwnRefusal(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the pool: %v", err)
	}
	ctx := context.Background()

	alice := person(t, ctx, "alice")
	bob := person(t, ctx, "bob")

	items, err := aiBusiness.AIActivity(ctx, false, &alice, 50)
	if err != nil {
		t.Fatalf("activity for a member: %v", err)
	}

	sawOwn, sawOther := false, false
	for _, it := range items {
		switch {
		case containsAll(it.Summary, "alice"):
			sawOwn = true
			if it.Status != "refused" {
				t.Errorf("alice's refusal arrived with status %q, so it renders like any "+
					"other entry", it.Status)
			}
		case containsAll(it.Summary, "bob"):
			sawOther = true
		}
	}

	if !sawOwn {
		t.Error("a member cannot see a decision recorded against them. They have to take " +
			"the guarantee on trust, which is what this product exists to replace.")
	}
	if sawOther {
		t.Error("a member can see ANOTHER member's audit entries. That is a privilege " +
			"escalation, and it would look exactly like the feature working.")
	}
	_ = bob
}

// And an admin still sees the workspace, or the change has traded one hole for
// another.
func TestAIActivityScopeStillShowsAnAdminEverything(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the pool: %v", err)
	}
	ctx := context.Background()

	alice := person(t, ctx, "alice")
	_ = person(t, ctx, "bob")

	items, err := aiBusiness.AIActivity(ctx, true, &alice, 50)
	if err != nil {
		t.Fatalf("activity for an admin: %v", err)
	}
	seen := map[string]bool{}
	for _, it := range items {
		if containsAll(it.Summary, "alice") {
			seen["alice"] = true
		}
		if containsAll(it.Summary, "bob") {
			seen["bob"] = true
		}
	}
	if !seen["alice"] || !seen["bob"] {
		t.Errorf("an admin sees %v, want both members' entries", seen)
	}
}

// A member with no principal must see nothing rather than everything: for a log,
// failing closed is the only safe direction.
func TestAIActivityScopeFailsClosedWithoutAPrincipal(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the pool: %v", err)
	}
	ctx := context.Background()
	_ = person(t, ctx, "alice")

	items, err := aiBusiness.AIActivity(ctx, false, nil, 50)
	if err != nil {
		t.Fatalf("activity with no principal: %v", err)
	}
	for _, it := range items {
		if it.Kind == "audit" {
			t.Fatalf("a caller with no principal received audit entry %q", it.Title)
		}
	}
}

func containsAll(s string, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
