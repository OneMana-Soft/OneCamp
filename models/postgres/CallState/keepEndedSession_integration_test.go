//go:build integration

package models_test

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	callState "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// KeepEndedSession writes a call that is over whole (the demo's Launch sync,
// business/DemoSeed). It never touches a call still going on that has the
// id, never writes a call with no id or no end after its start, and keeping
// the call again with its times moved moves it whole: its row, everyone's
// times in it, and the board it showed, still shown once, by whoever showed
// it this time and when.
func TestAKeptCallIsOneThatIsOverAndMovesWhole(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	const length = 25 * time.Minute
	maya, jonas := uuid.New(), uuid.New()
	began := time.Date(2026, 10, 30, 16, 5, 0, 0, time.UTC)
	// people is Maya and Jonas in a call from at for length: Maya hosts it
	// throughout, Jonas comes a minute late and is back two minutes after.
	people := func(at time.Time) []callState.Attendee {
		left := at.Add(length)
		return []callState.Attendee{
			{Identity: "maya", UserUUID: &maya, Name: "Maya Chen", Role: callState.RoleHost,
				JoinedAt: at, LastJoinedAt: at, LeftAt: &left, SecondsIn: int(length.Seconds())},
			{Identity: "jonas", UserUUID: &jonas, Name: "Jonas Berg", Role: callState.RoleParticipant,
				JoinedAt: at.Add(time.Minute), LastJoinedAt: at.Add(3 * time.Minute), LeftAt: &left, SecondsIn: int(length.Seconds()) - 120},
		}
	}
	over := func(id uuid.UUID, room string, at time.Time) callState.Session {
		end := at.Add(length)
		return callState.Session{ID: id, Room: room, RoomSID: "RM_" + room, Kind: callState.KindHuddle,
			ResourceKind: callState.ResourceGroup, ResourceID: room, StartedBy: &maya, StartedAt: at, EndedAt: &end,
			EndedReason: callState.EndedLastLeft}
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A call going on, with Maya in it and a board on its stage.
	live := callState.Session{ID: uuid.New(), Room: "group-live", Kind: callState.KindHuddle,
		ResourceKind: callState.ResourceGroup, ResourceID: "group-live", StartedBy: &maya, StartedAt: began}
	if _, made, err := callState.OpenSession(ctx, live); err != nil || !made {
		t.Fatalf("opening the live call: made %v, %v", made, err)
	}
	if first, _, err := callState.Joined(ctx, callState.Attendee{SessionID: live.ID, Identity: "maya", UserUUID: &maya,
		Name: "Maya Chen", Role: callState.RoleHost}, began); err != nil || !first {
		t.Fatalf("Maya joining the live call: first %v, %v", first, err)
	}
	liveBoard := uuid.New()
	if on, _, err := callState.SetBoard(ctx, live.ID, liveBoard, "maya", false); err != nil || !on {
		t.Fatalf("showing a board on the live call: on %v, %v", on, err)
	}
	// Its id kept as a call that is over, an hour earlier, with Jonas in it
	// and another board: refused, and the call, its people and its boards
	// are as they were.
	if err := callState.KeepEndedSession(ctx, over(live.ID, "group-live", began.Add(-time.Hour)), people(began.Add(-time.Hour)),
		[]callState.ShownBoard{{Board: uuid.New(), By: "jonas", At: began.Add(-time.Hour)}}); err == nil {
		t.Error("a call still going on was kept as one that is over")
	}
	if s, err := callState.SessionByID(ctx, live.ID); err != nil || s == nil || !s.Open() || !s.StartedAt.Equal(began) {
		t.Errorf("the live call is %+v (%v), want it going on since %v", s, err, began)
	}
	if in, err := callState.Attendees(ctx, live.ID); err != nil || len(in) != 1 || in[0].Identity != "maya" || !in[0].In() || !in[0].JoinedAt.Equal(began) {
		t.Errorf("the live call's people are %+v (%v), want Maya alone, in since %v", in, err, began)
	}
	if boards, err := callState.BoardsShown(ctx, live.ID); err != nil || len(boards) != 1 || boards[0] != liveBoard {
		t.Errorf("the live call showed %v (%v), want only %v", boards, err, liveBoard)
	}

	// No id, no end, or an end before its start: refused, nothing written.
	noEnd, backwards := over(uuid.New(), "group-kept", began), over(uuid.New(), "group-kept", began)
	noEnd.EndedAt = nil
	before := began.Add(-time.Minute)
	backwards.EndedAt = &before
	for _, c := range []struct {
		name string
		s    callState.Session
	}{{"no id", over(uuid.Nil, "group-kept", began)}, {"no end", noEnd}, {"an end before its start", backwards}} {
		if err := callState.KeepEndedSession(ctx, c.s, people(began), []callState.ShownBoard{{Board: uuid.New(), By: "maya", At: began}}); err == nil {
			t.Errorf("a call with %s was kept", c.name)
		}
	}
	if s, a, b := count("call_sessions"), count("call_attendance"), count("call_session_boards"); s != 1 || a != 1 || b != 1 {
		t.Errorf("%d calls, %d people in them and %d boards shown; want the live call's 1, 1 and 1", s, a, b)
	}

	// A call that is over, kept, then kept again with its times moved and the
	// same board shown by Jonas this time: one call at the new times, each
	// person's times the new ones, the board shown once, by Jonas, then.
	kept, board := uuid.New(), uuid.New()
	keep := func(at time.Time, by string) {
		t.Helper()
		if err := callState.KeepEndedSession(ctx, over(kept, "group-kept", at), people(at),
			[]callState.ShownBoard{{Board: board, By: by, At: at.Add(2 * time.Minute)}}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string, at time.Time, by string) {
		t.Helper()
		s, err := callState.SessionByID(ctx, kept)
		if err != nil || s == nil || s.Open() || !s.StartedAt.Equal(at) || !s.EndedAt.Equal(at.Add(length)) || s.RoomSID != "RM_group-kept" {
			t.Fatalf("%s: the call is %+v (%v), want it from %v to %v", when, s, err, at, at.Add(length))
		}
		got, err := callState.Attendees(ctx, kept)
		want := people(at)
		if err != nil || len(got) != len(want) {
			t.Fatalf("%s: %d people in the call (%v), want %d", when, len(got), err, len(want))
		}
		for i, w := range want {
			g := got[i]
			if g.Identity != w.Identity || g.Name != w.Name || g.Role != w.Role || g.UserUUID == nil || *g.UserUUID != *w.UserUUID ||
				!g.JoinedAt.Equal(w.JoinedAt) || !g.LastJoinedAt.Equal(w.LastJoinedAt) || g.LeftAt == nil || !g.LeftAt.Equal(*w.LeftAt) ||
				g.SecondsIn != w.SecondsIn {
				t.Errorf("%s: %s in the call is %+v, want %+v", when, w.Identity, g, w)
			}
		}
		rows, err := env.PG.Query(`SELECT board_uuid, shown_by, first_shown_at FROM call_session_boards WHERE session_id = $1`, kept)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var shown uuid.UUID
			var shownBy string
			var first time.Time
			if err := rows.Scan(&shown, &shownBy, &first); err != nil {
				t.Fatal(err)
			}
			n++
			if shown != board || shownBy != by || !first.Equal(at.Add(2*time.Minute)) {
				t.Errorf("%s: the call showed %v by %q at %v, want %v by %q at %v", when, shown, shownBy, first, board, by, at.Add(2*time.Minute))
			}
		}
		if err := rows.Err(); err != nil || n != 1 {
			t.Errorf("%s: the call showed %d boards (%v), want 1", when, n, err)
		}
	}
	keep(began, "maya")
	check("kept", began, "maya")
	moved := began.Add(24*time.Hour + 30*time.Minute)
	keep(moved, "jonas")
	check("kept again, moved", moved, "jonas")
	if n := count("call_sessions"); n != 2 {
		t.Errorf("%d calls, want the live one and the kept one", n)
	}
}
