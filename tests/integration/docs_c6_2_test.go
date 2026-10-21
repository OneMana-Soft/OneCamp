//go:build integration
// +build integration

package integration_test

// The workspace's longest link lifetime and the links of people who left
// (docs C6-2), against real Postgres 12: the one live-grant predicate (the
// model's liveSQL) stops a link made the cap's days ago or more, leaves a
// personal room alone, and brings links back when the cap is raised, with
// nothing written; how many a shorter cap would stop is counted as the
// predicate would stop them; and a departed member's links move to someone,
// or are turned off, found by who made them and who published them. The Go
// side of the predicate is business/Guest grantExpired, held to the same
// cases by kinds_test.go.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestLinkLifetime|TestDepartedLinks|TestDocsOwned' -v

import (
	"context"
	"crypto/sha256"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	departedModel "github.com/akashc777/OneCamp/models/postgres/Departed"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/tests/integration"
)

// c62Grant is one row of guest_grants as these tests make them.
type c62Grant struct {
	name      string
	kind      string
	slug      string
	madeDays  int // made this many days ago
	by        uuid.UUID
	published uuid.UUID
}

func c62Setup(t *testing.T) (*integration.Env, map[string]uuid.UUID, map[string]uuid.UUID) {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	prev := guestModel.LinkCapDays
	t.Cleanup(func() { guestModel.LinkCapDays = prev })
	people := map[string]uuid.UUID{}
	for _, name := range []string{"owner", "dana", "maya"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, name+"-"+id.String()[:8]+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	// Dana has left.
	if _, err := env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, people["dana"]); err != nil {
		t.Fatal(err)
	}
	grants := map[string]uuid.UUID{}
	for _, g := range []c62Grant{
		{name: "old-page", kind: "doc", slug: "old-page", madeDays: 40, by: people["owner"], published: people["owner"]},
		{name: "new-link", kind: "doc", madeDays: 10, by: people["owner"]},
		{name: "room", kind: "meeting", slug: "owner-room", madeDays: 400, by: people["owner"]},
		{name: "dana-link", kind: "doc", madeDays: 5, by: people["dana"]},
		{name: "dana-page", kind: "doc", slug: "dana-page", madeDays: 5, by: people["owner"], published: people["dana"]},
	} {
		id := uuid.New()
		grants[g.name] = id
		hash := sha256.Sum256([]byte(g.name))
		var slug, published any
		if g.slug != "" {
			slug = g.slug
		}
		if g.published != uuid.Nil {
			published = g.published
		}
		capability := "view"
		if g.kind == "meeting" {
			capability = "join"
		}
		if _, err := env.PG.Exec(`INSERT INTO guest_grants (id, token_hash, resource_type, resource_id, capability, created_by, expires_at, created_at, slug, published_by)
			VALUES ($1, $2, $3, $4, $5, $6, NOW() + interval '1000 days', NOW() - make_interval(days => $7), $8, $9)`,
			id, hash[:], g.kind, uuid.NewString(), capability, g.by, g.madeDays, slug, published); err != nil {
			t.Fatal(err)
		}
	}
	return env, people, grants
}

func liveNames(t *testing.T, grants map[string]uuid.UUID) []string {
	t.Helper()
	rows, err := guestModel.ListActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uuid.UUID]string{}
	for name, id := range grants {
		byID[id] = name
	}
	var out []string
	for _, g := range rows {
		if n, ok := byID[g.Id]; ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// setCap sets the workspace's guest-link cap for this test and puts the
// real one back when it ends.
func setCap(t *testing.T, days int) {
	t.Helper()
	prev := guestModel.LinkCapDays
	t.Cleanup(func() { guestModel.LinkCapDays = prev })
	guestModel.LinkCapDays = func(context.Context) int { return days }
}

func TestLinkLifetimeInTheOnePredicate(t *testing.T) {
	ctx := context.Background()
	_, _, grants := c62Setup(t)
	all := "dana-link,dana-page,new-link,old-page,room"

	setCap(t, 0)
	if got := strings.Join(liveNames(t, grants), ","); got != all {
		t.Fatalf("no cap: %s", got)
	}
	// How many a 30-day cap would stop now: the page made 40 days ago. The
	// room is standing.
	if n, err := guestModel.CountStoppedByCap(ctx, 30); err != nil || n != 1 {
		t.Fatalf("30 days would stop %d, %v", n, err)
	}
	setCap(t, 30)
	if got := strings.Join(liveNames(t, grants), ","); got != "dana-link,dana-page,new-link,room" {
		t.Fatalf("a 30-day cap: %s", got)
	}
	hash := sha256.Sum256([]byte("old-page"))
	if g, err := guestModel.GetActiveByHash(ctx, hash[:]); err != nil || g != nil {
		t.Fatalf("its token still opens: %v %v", g, err)
	}
	setCap(t, 7)
	if got := strings.Join(liveNames(t, grants), ","); got != "dana-link,dana-page,room" {
		t.Fatalf("a 7-day cap: %s", got)
	}
	// Raised again: back, since nothing was written.
	setCap(t, 365)
	if got := strings.Join(liveNames(t, grants), ","); got != all {
		t.Fatalf("a year's cap: %s", got)
	}
	if n, err := guestModel.CountStoppedByCap(ctx, 7); err != nil || n != 2 {
		t.Fatalf("7 days would stop %d, %v", n, err)
	}
}

func TestDepartedLinksMoveOrStop(t *testing.T) {
	ctx := context.Background()
	env, people, grants := c62Setup(t)
	setCap(t, 0)

	gone, err := departedModel.Departed(ctx, people["dana"])
	if err != nil || gone == nil {
		t.Fatalf("Dana has left: %v %v", gone, err)
	}
	if still, _ := departedModel.Departed(ctx, people["maya"]); still != nil {
		t.Fatal("Maya hasn't left")
	}
	if ok, _, err := departedModel.ActiveMember(ctx, people["dana"]); err != nil || ok {
		t.Fatalf("someone who left is no one to hand to: %v %v", ok, err)
	}
	// Dana's personal room: standing, at her name.
	room := uuid.New()
	roomHash := sha256.Sum256([]byte("dana-room"))
	if _, err := env.PG.Exec(`INSERT INTO guest_grants (id, token_hash, resource_type, resource_id, capability, created_by, created_at, slug)
		VALUES ($1, $2, 'meeting', $3, 'join', $4, NOW() - interval '3 days', 'dana-ross')`, room, roomHash[:], uuid.NewString(), people["dana"]); err != nil {
		t.Fatal(err)
	}
	// Counted for several people in one read: Dana's two and her room, and
	// nobody else's (Maya has none of her own yet, so she is left out).
	if n, err := guestModel.CountLiveByPeople(ctx, []uuid.UUID{people["dana"], people["maya"]}); err != nil || len(n) != 1 || n[people["dana"]] != 3 {
		t.Fatalf("Dana's links: %v %v", n, err)
	}

	// Her links move; her room closes, since it is hers (D-CL-35).
	moved, closed, err := guestModel.HandOverLinks(ctx, people["dana"], people["maya"])
	if err != nil || moved != 2 || closed != 1 {
		t.Fatalf("moved %d, closed %d, %v", moved, closed, err)
	}
	var roomBy uuid.UUID
	var roomEnded bool
	if err := env.PG.QueryRow(`SELECT created_by, revoked_at IS NOT NULL FROM guest_grants WHERE id = $1`, room).Scan(&roomBy, &roomEnded); err != nil || roomBy != people["dana"] || !roomEnded {
		t.Fatalf("Dana's room: by %s, ended %v, %v", roomBy, roomEnded, err)
	}
	if n, err := guestModel.CountLiveByPeople(ctx, []uuid.UUID{people["dana"]}); err != nil || len(n) != 0 {
		t.Fatalf("after the handover, nothing is under Dana's name: %v %v", n, err)
	}
	var by, pub uuid.UUID
	if err := env.PG.QueryRow(`SELECT created_by FROM guest_grants WHERE id = $1`, grants["dana-link"]).Scan(&by); err != nil || by != people["maya"] {
		t.Fatalf("Dana's link is %s's", by)
	}
	if err := env.PG.QueryRow(`SELECT published_by FROM guest_grants WHERE id = $1`, grants["dana-page"]).Scan(&pub); err != nil || pub != people["maya"] {
		t.Fatalf("Dana's page was published by %s", pub)
	}
	if got := strings.Join(liveNames(t, grants), ","); got != "dana-link,dana-page,new-link,old-page,room" {
		t.Fatalf("handed on, they keep working: %s", got)
	}

	// Turned off: everything made or published by the person, and nothing else.
	n, err := guestModel.RevokeLiveByPerson(ctx, people["maya"])
	if err != nil || n != 2 {
		t.Fatalf("turned off %d, %v", n, err)
	}
	if got := strings.Join(liveNames(t, grants), ","); got != "new-link,old-page,room" {
		t.Fatalf("after turning Maya's off: %s", got)
	}
}

// Who owns how many docs, counted for everyone who left in one read
// (domain/Doc DocsOwnedCounts), by DocsOwnedBy's rule: a doc's owner, or its
// maker while it has none, live or in the trash.
func TestDocsOwnedCountsByTheOwnerRule(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	t.Cleanup(dg.Close)
	ana, ben, cy := uuid.New(), uuid.New(), uuid.New()
	doc := func(owner, maker string, extra map[string]any) map[string]any {
		n := map[string]any{"dgraph.type": "Doc", "doc_uuid": uuid.NewString(), "doc_created_by": map[string]any{"uid": maker}}
		if owner != "" {
			n["doc_owner"] = map[string]any{"uid": owner}
		}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String()},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": ben.String()},
		{"uid": "_:cy", "dgraph.type": "User", "user_uuid": cy.String()},
		doc("_:ana", "_:ana", nil),
		// Made by Ana with no owner: hers.
		doc("", "_:ana", nil),
		// Made by Ana, owned by Ben: his.
		doc("_:ben", "_:ana", nil),
		// In the trash: still Ana's, since its owner restores it.
		doc("_:ana", "_:ana", map[string]any{"doc_deleted_at": "2026-10-01T00:00:00Z"}),
	})
	counts, err := docDomain.DocsOwnedCounts(ctx, []uuid.UUID{ana, ben, cy})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{strings.ToLower(ana.String()): 3, strings.ToLower(ben.String()): 1}
	if len(counts) != len(want) || counts[strings.ToLower(ana.String())] != 3 || counts[strings.ToLower(ben.String())] != 1 {
		t.Fatalf("counts %v, want %v (Cy owns none and is left out)", counts, want)
	}
	// One person at a time, DocsOwnedBy says the same.
	for _, who := range []uuid.UUID{ana, ben, cy} {
		ids, n, err := docDomain.DocsOwnedBy(ctx, who.String(), 10)
		if err != nil || n != counts[strings.ToLower(who.String())] || len(ids) != n {
			t.Fatalf("%s: DocsOwnedBy %d (%d ids), %v; counts say %d", who, n, len(ids), err, counts[strings.ToLower(who.String())])
		}
	}
	if none, err := docDomain.DocsOwnedCounts(ctx, nil); err != nil || len(none) != 0 {
		t.Fatalf("nobody: %v %v", none, err)
	}
}
