//go:build integration
// +build integration

package integration_test

// Handles for members from before handles (business/User.BackfillHandles),
// against Postgres 12 and Dgraph: each member without one gets the handle
// sign up would have given them, from their display name, else their full
// name, else their address, numbered past one taken; bots, external rows,
// deleted accounts and members with a handle are left as they are; two
// with the same name are numbered in the order they joined, which the run
// walks a page at a time, each member once; and a second run finds nobody. Of replicas starting together one runs it: while
// another's run holds its lock, a run gives nobody a handle, and a run lets
// the lock go when it ends.
//
// Run: go test -tags=integration ./tests/integration/ -run TestHandleBackfill -v

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestHandleBackfill(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)

	type row struct {
		email, username        string
		external, bot, deleted bool
		displayName, fullName  string
		inGraph                bool
		id                     string // fixed, else a new one
		joined                 string // when they joined, else now
	}
	people := map[string]row{
		"taken":    {email: "first.sam@example.test", username: "sam-rivera", displayName: "Sam Rivera", inGraph: true},
		"sam":      {email: "sam@example.test", displayName: "Sam Rivera", fullName: "Samuel Rivera", inGraph: true},
		"full":     {email: "priya@example.test", fullName: "Priya Raman", inGraph: true, joined: "2025-10-01T09:00:00Z"},
		"address":  {email: "jo.chen@example.test", joined: "2025-10-01T09:00:00Z"}, // the same moment as full
		"bot":      {email: "bot-x@bots.example.test", bot: true, external: true, displayName: "Release Captain", inGraph: true},
		"external": {email: "octo@example.test", external: true, displayName: "Octo Cat", inGraph: true},
		"deleted":  {email: "gone@example.test", deleted: true, displayName: "Gone Person", inGraph: true},
		// Two Jo Parks: the one who joined first sorts last by id.
		"firstJo": {email: "jo.park@example.test", displayName: "Jo Park", inGraph: true,
			id: "ffffffff-ffff-4fff-bfff-ffffffffffff", joined: "2025-09-01T09:00:00Z"},
		"laterJo": {email: "jpark@example.test", displayName: "Jo Park", inGraph: true,
			id: "00000000-0000-4000-8000-000000000001"},
	}
	ids := map[string]uuid.UUID{}
	var nodes []map[string]any
	for key, p := range people {
		id := uuid.New()
		if p.id != "" {
			id = uuid.MustParse(p.id)
		}
		ids[key] = id
		var username any
		if p.username != "" {
			username = p.username
		}
		var deletedAt, joined any
		if p.deleted {
			deletedAt = "2026-01-01T00:00:00Z"
		}
		if p.joined != "" {
			joined = p.joined
		}
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, is_external, is_bot, deleted_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::timestamptz, NOW()), NOW())`, id, p.email, username, p.external, p.bot, deletedAt, joined); err != nil {
			t.Fatal(err)
		}
		if p.inGraph {
			node := map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_email_id": p.email}
			if p.displayName != "" {
				node["user_name"] = p.displayName
			}
			if p.fullName != "" {
				node["user_full_name"] = p.fullName
			}
			nodes = append(nodes, node)
		}
	}
	dg.Mutate(t, nodes)

	// The walk, one member a page so that every step, the tie included, is
	// a page boundary: everyone without a handle once, in join order, then
	// by id.
	var walked []userDomain.MemberWithoutHandle
	var after userDomain.MemberWithoutHandle
	for len(walked) <= len(people) {
		page, err := userDomain.MembersWithoutHandle(ctx, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, page...)
		after = page[len(page)-1]
	}
	seen := map[uuid.UUID]bool{}
	for i, m := range walked {
		seen[m.ID] = true
		if i > 0 {
			prev := walked[i-1]
			if !prev.JoinedAt.Before(m.JoinedAt) && !(prev.JoinedAt.Equal(m.JoinedAt) && prev.ID.String() < m.ID.String()) {
				t.Errorf("the walk went from %s (%s) to %s (%s)", prev.ID, prev.JoinedAt, m.ID, m.JoinedAt)
			}
		}
	}
	if len(walked) != 5 || len(seen) != 5 || !seen[ids["sam"]] || !seen[ids["full"]] || !seen[ids["address"]] || !seen[ids["firstJo"]] || !seen[ids["laterJo"]] {
		t.Fatalf("the walk found %v, want sam, full, address and both Jo Parks once each", walked)
	}

	handle := func(key string) string {
		var h sql.NullString
		if err := env.PG.QueryRow(`SELECT username FROM users WHERE id = $1`, ids[key]).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h.String
	}
	// lock takes the backfill's lock, as another replica's run would, and
	// answers whether it was free; unlock lets it go.
	held, err := env.PG.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	lock := func() bool {
		var got bool
		if err := held.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, userBusiness.HandleBackfillLockKey).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	unlock := func() {
		if _, err := held.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, userBusiness.HandleBackfillLockKey); err != nil {
			t.Fatal(err)
		}
	}

	if !lock() {
		t.Fatal("the backfill's lock was taken before any run")
	}
	given, failed, err := userBusiness.BackfillHandles(ctx)
	if err != nil || given != 0 || failed != 0 || handle("sam") != "" {
		t.Fatalf("a run while another holds the lock gave %d, failed %d (%v), sam has %q; want it skipped", given, failed, err, handle("sam"))
	}
	unlock()

	given, failed, err = userBusiness.BackfillHandles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if given != 5 || failed != 0 {
		t.Fatalf("gave %d handles and failed %d, want 5 and 0", given, failed)
	}
	if !lock() {
		t.Fatal("a finished run kept its lock")
	}
	unlock()
	for key, want := range map[string]string{
		"taken":    "sam-rivera",   // kept
		"sam":      "sam-rivera-2", // display name, numbered past the one taken
		"full":     "priya-raman",  // no display name: the full name
		"address":  "jo.chen",      // no names: the address
		"firstJo":  "jo-park",      // joined first
		"laterJo":  "jo-park-2",    // joined later, though their id sorts first
		"bot":      "",             // bots keep their own scheme
		"external": "",
		"deleted":  "",
	} {
		if got := handle(key); got != want {
			t.Errorf("%s: handle %q, want %q", key, got, want)
		}
	}

	given, failed, err = userBusiness.BackfillHandles(ctx)
	if err != nil || given != 0 || failed != 0 {
		t.Fatalf("a second run: gave %d, failed %d, err %v; want nothing to do", given, failed, err)
	}
}
