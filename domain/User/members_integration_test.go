//go:build integration

package domain_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// The admin's members list is a page at most its size, with how many match in
// all, searched by name or address and narrowed to admins or members on the
// server; guests are never in it.
func TestTheMembersListIsSearchedAndCountedOnTheServer(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	var nodes []map[string]any
	for i := 0; i < 12; i++ {
		id := uuid.New()
		email := fmt.Sprintf("member%02d@example.test", i)
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, id, email); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ($1)`, email); err != nil {
				t.Fatal(err)
			}
		}
		name := fmt.Sprintf("member%02d", i)
		if i == 7 {
			name = "Zelda"
		}
		nodes = append(nodes, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_email_id": email,
			"user_created_at": fmt.Sprintf("2026-10-%02dT10:00:00Z", i+1)})
	}
	nodes = append(nodes, map[string]any{"dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "guest", "is_external": true})
	dg.Mutate(t, nodes)

	page, total, err := userDomain.GetMembersPage(ctx, 5, 0, userDomain.MembersFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 5 || total != 12 {
		t.Fatalf("first page: %d people of %d; want 5 of 12 (guests left out)", len(page), total)
	}
	if page[0].UserName != "member11" {
		t.Errorf("newest first: %s", page[0].UserName)
	}
	last, _, _ := userDomain.GetMembersPage(ctx, 5, 10, userDomain.MembersFilter{})
	if len(last) != 2 {
		t.Errorf("last page: %d, want 2", len(last))
	}
	byName, total, _ := userDomain.GetMembersPage(ctx, 5, 0, userDomain.MembersFilter{Q: "zel"})
	if total != 1 || len(byName) != 1 || byName[0].UserName != "Zelda" {
		t.Errorf("by name: %d %v", total, byName)
	}
	if _, total, _ := userDomain.GetMembersPage(ctx, 5, 0, userDomain.MembersFilter{Q: "member03@"}); total != 1 {
		t.Errorf("by address: %d", total)
	}
	yes, no := true, false
	admins, total, _ := userDomain.GetMembersPage(ctx, 50, 0, userDomain.MembersFilter{Admins: &yes})
	var names []string
	for _, a := range admins {
		names = append(names, a.UserName)
	}
	slices.Sort(names)
	if total != 2 || !slices.Equal(names, []string{"member00", "member01"}) {
		t.Errorf("admins: %d %v", total, names)
	}
	if _, total, _ := userDomain.GetMembersPage(ctx, 50, 0, userDomain.MembersFilter{Admins: &no}); total != 10 {
		t.Errorf("everyone but the admins: %d", total)
	}
}
