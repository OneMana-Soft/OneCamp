//go:build integration

package integration_test

// People outside the workspace are never shown a member's address (27 Oct
// 2026). An instant meeting's guests were told they were joining
// "visitor@demo.onemana.dev's meeting", and a personal room's public address
// was made from its owner's email: both read the name the admin screens use,
// which falls back to the address, and a member's name lives in the graph
// (users.display_name is written only for bots and imported people), so for
// most members that name was their email.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A meeting's guests are told whose it is by the name its host goes by in the
// call, never by their address: "Meeting" when there is no name to give, or
// the only one is an address. A room's address is made the same way.
func TestAMeetingsGuestsAreNeverShownItsHostsAddress(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}

	// Members as the app writes them: a name in the graph, none in users.
	sam, nameless, mailed := uuid.New(), uuid.New(), uuid.New()
	emails := map[uuid.UUID]string{sam: "sam.rivera@example.test", nameless: "nobody@example.test", mailed: "kai@example.test"}
	for id, email := range emails {
		if _, err := pg.PG.Exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())`, id, email); err != nil {
			t.Fatal(err)
		}
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "Sam Rivera", "user_email_id": emails[sam]},
		{"uid": "_:nameless", "dgraph.type": "User", "user_uuid": nameless.String(), "user_email_id": emails[nameless]},
		{"uid": "_:mailed", "dgraph.type": "User", "user_uuid": mailed.String(), "user_name": emails[mailed], "user_email_id": emails[mailed]},
	})
	meeting := func() string { return "meet-" + uuid.NewString() }

	if got := callLobbyBusiness.Title(ctx, meeting(), sam); got != "Sam Rivera's meeting" {
		t.Errorf("Sam's meeting reads %q to a guest, want \"Sam Rivera's meeting\"", got)
	}
	for _, id := range []uuid.UUID{nameless, mailed} {
		if got := callLobbyBusiness.Title(ctx, meeting(), id); got != "Meeting" {
			t.Errorf("%s's meeting reads %q to a guest, want \"Meeting\"", emails[id], got)
		}
	}

	slugOf := func(g *guestModel.GuestGrant) string {
		if g == nil || g.Slug == nil {
			return ""
		}
		return *g.Slug
	}
	room, err := guestBusiness.NewRoom(ctx, meeting(), sam)
	if err != nil || slugOf(room) != "sam-rivera" {
		t.Fatalf("Sam's room stands at %q (%v), want sam-rivera", slugOf(room), err)
	}
	rooms := map[uuid.UUID]uuid.UUID{sam: room.Id}
	for _, id := range []uuid.UUID{nameless, mailed} {
		room, err := guestBusiness.NewRoom(ctx, meeting(), id)
		if err != nil || slugOf(room) == "" {
			t.Fatalf("%s's room: %+v, %v", emails[id], room, err)
		}
		rooms[id] = room.Id
		local := strings.SplitN(emails[id], "@", 2)[0]
		if slug := slugOf(room); strings.Contains(slug, local) || strings.Contains(slug, "example") {
			t.Errorf("%s's room stands at %q, made from their address", emails[id], slug)
		}
	}

	// Rooms made before: at their owner's email's words, alone or with the
	// random letters a taken address got. The start-up job moves them to the
	// address a new room gets, once; a room whose owner chose its address stays.
	slugIs := func(id uuid.UUID, slug string) {
		t.Helper()
		if _, err := pg.PG.Exec(`UPDATE guest_grants SET slug = $2 WHERE id = $1`, id, slug); err != nil {
			t.Fatal(err)
		}
	}
	slugNow := func(id uuid.UUID) string {
		t.Helper()
		var slug string
		if err := pg.PG.QueryRow(`SELECT slug FROM guest_grants WHERE id = $1`, id).Scan(&slug); err != nil {
			t.Fatal(err)
		}
		return slug
	}
	slugIs(rooms[sam], "sam-riveraexample-test")
	// Sam has since set a display name (CR-65 SF1): where a room's address came
	// from is said by the owner's email itself, not by the name the admin
	// screens read, which is now the display name.
	if _, err := pg.PG.Exec(`UPDATE users SET display_name = 'Sam Rivera' WHERE id = $1`, sam); err != nil {
		t.Fatal(err)
	}
	slugIs(rooms[nameless], "nobodyexample-test-1a2b3c4d")
	slugIs(rooms[mailed], "kais-standup")
	for pass := 1; pass <= 2; pass++ {
		if err := guestBusiness.MoveRoomsOffOwnersAddresses(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got := slugNow(rooms[sam]); got != "sam-rivera" {
			t.Errorf("pass %d: Sam's old room stands at %q, want sam-rivera", pass, got)
		}
		if got := slugNow(rooms[nameless]); got != "room" {
			t.Errorf("pass %d: the nameless owner's old room stands at %q, want room", pass, got)
		}
		if got := slugNow(rooms[mailed]); got != "kais-standup" {
			t.Errorf("pass %d: a room at an address its owner chose moved to %q", pass, got)
		}
	}
}
