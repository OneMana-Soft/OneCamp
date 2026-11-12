//go:build integration

package business

// V-581 (C11; D2 moved here from V-577, its red first written by CHAT35-C10):
// an "Overdue commitment" nudge stayed open and urgent for five months on the
// owner's account, about a commitment overdue since June. Each pass of the
// engine now settles its open nudges first, whether or not nudges are being
// made: a nudge whose commitment no longer needs its person now (done, gone,
// someone else's, or stale: overdue for more than 14 days) is resolved, and
// an urgent nudge not raised again for 14 days drops to priority 0, still
// listed. Nothing is deleted. And the pass makes nudges only of commitments
// that need their person now (the same predicate as the home's attention
// list): never of an undated one, nor a stale one.
//
// Run: go test -tags=integration ./business/AI/ -run TestTheEngineSettlesStaleNudges -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	nudgeModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheEngineSettlesStaleNudges(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)

	owner, maya := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"owner": owner, "maya": maya} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	// commitment writes a memory commitment owned by whom, overdue since
	// yesterday, in status, and deleted when said.
	commitment := func(t *testing.T, whom uuid.UUID, status string, deleted bool) string {
		t.Helper()
		var id string
		if err := env.PG.QueryRow(`INSERT INTO workspace_memory_items (kind, content, status, owner_user_id, due_at, dedup_hash, deleted_at)
			VALUES ('commitment', 'Send the launch numbers', $1, $2, NOW() - interval '1 day', $3, CASE WHEN $4 THEN NOW() END)
			RETURNING id`, status, whom, uuid.NewString(), deleted).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// nudge writes the owner's open nudge of kind about source, made made ago
	// and last raised raised ago.
	nudge := func(t *testing.T, kind, sourceType, source string, priority int, made, raised time.Duration) string {
		t.Helper()
		var id string
		if err := env.PG.QueryRow(`INSERT INTO workspace_nudges
			(user_id, kind, title, body, source_type, source_id, status, priority, dedup_key, created_at, updated_at)
			VALUES ($1, $2, 'Overdue commitment', 'You committed to: send the launch numbers.', $3, $4, 'open', $5, $6, $7, $8)
			RETURNING id`, owner, kind, sourceType, source, priority, "memory:"+source, time.Now().Add(-made), time.Now().Add(-raised)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	type state struct {
		status   string
		priority int
		raised   time.Time
	}
	read := func(t *testing.T, id string) state {
		t.Helper()
		var s state
		if err := env.PG.QueryRow(`SELECT status, priority, updated_at FROM workspace_nudges WHERE id = $1`, id).Scan(&s.status, &s.priority, &s.raised); err != nil {
			t.Fatal(err)
		}
		return s
	}

	day := 24 * time.Hour
	open := nudge(t, nudgeModels.KindOverdueCommitment, "memory", commitment(t, owner, "open", false), 1, day, day)
	done := nudge(t, nudgeModels.KindOverdueCommitment, "memory", commitment(t, owner, "resolved", false), 1, day, day)
	deleted := nudge(t, nudgeModels.KindOverdueCommitment, "memory", commitment(t, owner, "open", true), 1, day, day)
	missing := nudge(t, nudgeModels.KindOverdueCommitment, "memory", uuid.NewString(), 1, day, day)
	moved := nudge(t, nudgeModels.KindOverdueCommitment, "memory", commitment(t, maya, "open", false), 1, day, day)
	staleSource := commitment(t, owner, "open", false)
	stale := nudge(t, nudgeModels.KindOverdueCommitment, "memory", staleSource, 1, 150*day, 15*day)
	reraised := nudge(t, nudgeModels.KindOverdueCommitment, "memory", commitment(t, owner, "open", false), 1, 20*day, 2*day)
	unraised13 := nudge(t, nudgeModels.KindOverdueCommitment, "memory", commitment(t, owner, "open", false), 1, 13*day, 13*day)
	overdue := func(t *testing.T, days int) string {
		t.Helper()
		var id string
		if err := env.PG.QueryRow(`INSERT INTO workspace_memory_items (kind, content, status, owner_user_id, due_at, dedup_hash)
			VALUES ('commitment', 'Send the launch numbers', 'open', $1, NOW() - make_interval(days => $2), $3)
			RETURNING id`, owner, days, uuid.NewString()).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	staleCommitment := nudge(t, nudgeModels.KindOverdueCommitment, "memory", overdue(t, 15), 1, 15*day, day)
	question := nudge(t, nudgeModels.KindStaleQuestion, "memory", uuid.NewString(), 0, day, day)
	staleBefore := read(t, stale).raised
	// The last pass found the stale nudge's signal again, as every hourly pass
	// does while its commitment stays overdue. Finding it again is not raising
	// it (C10's updated_at, V-577), so it still goes quiet below; were each
	// pass a raise, a nudge whose signal holds would never go quiet.
	in := nudgeModels.UpsertInput{UserID: owner, Kind: nudgeModels.KindOverdueCommitment, Title: "Overdue commitment",
		Body: "You committed to: send the launch numbers.", SourceType: "memory", SourceID: staleSource, Priority: 1, DedupKey: "memory:" + staleSource}
	if _, err := nudgeModels.UpsertMany(ctx, owner, []nudgeModels.UpsertInput{in}); err != nil {
		t.Fatal(err)
	}

	runNudgePass(ctx)

	t.Run("a nudge whose commitment is done or gone is resolved", func(t *testing.T) {
		for name, id := range map[string]string{"done": done, "deleted": deleted, "missing": missing, "now maya's": moved, "overdue 15 days": staleCommitment} {
			if s := read(t, id); s.status != nudgeModels.StatusSuperseded {
				t.Errorf("the nudge for a commitment %s is %q; want %q: the commitment behind it is no longer the owner's to do", name, s.status, nudgeModels.StatusSuperseded)
			}
		}
		if s := read(t, open); s.status != nudgeModels.StatusOpen || s.priority != 1 {
			t.Errorf("the nudge for an open overdue commitment is %q at priority %d; want open and urgent", s.status, s.priority)
		}
		if s := read(t, question); s.status != nudgeModels.StatusOpen {
			t.Errorf("a stale question's nudge is %q; want open: only overdue commitments are checked against their commitment", s.status)
		}
		var rows int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM workspace_nudges WHERE user_id = $1`, owner).Scan(&rows); err != nil || rows != 10 {
			t.Errorf("the owner has %d nudge rows (%v); want all 10: nothing is deleted", rows, err)
		}
	})

	t.Run("an urgent nudge not raised again for 14 days goes quiet, still listed", func(t *testing.T) {
		s := read(t, stale)
		if s.status != nudgeModels.StatusOpen || s.priority != 0 {
			t.Errorf("a nudge last raised 15 days ago is %q at priority %d; want open at 0", s.status, s.priority)
		}
		if !s.raised.Equal(staleBefore) {
			t.Errorf("going quiet moved its raise from %v to %v; want it left: going quiet isn't a raise", staleBefore, s.raised)
		}
		if r := read(t, reraised); r.status != nudgeModels.StatusOpen || r.priority != 1 {
			t.Errorf("a nudge made 20 days ago and raised again 2 days ago is %q at priority %d; want open and urgent", r.status, r.priority)
		}
		if r := read(t, unraised13); r.status != nudgeModels.StatusOpen || r.priority != 1 {
			t.Errorf("a nudge last raised 13 days ago is %q at priority %d; want open and urgent: it goes quiet only after 14", r.status, r.priority)
		}
		listed, err := nudgeModels.ListOpenForUser(ctx, owner, 50)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, n := range listed {
			found = found || n.ID.String() == stale
		}
		if !found {
			t.Errorf("the quiet nudge isn't listed; want it listed until it is resolved or dismissed")
		}
	})

	t.Run("the engine finding its signal again leaves it quiet; raising it again makes it urgent", func(t *testing.T) {
		if _, err := nudgeModels.UpsertMany(ctx, owner, []nudgeModels.UpsertInput{in}); err != nil {
			t.Fatal(err)
		}
		// When a pass raises a nudge is CHAT35-C10's (V-577); its priority is
		// this head's: a pass never makes a quiet nudge urgent again.
		if s := read(t, stale); s.priority != 0 || !s.raised.Equal(staleBefore) {
			t.Errorf("after an engine pass the quiet nudge is at priority %d, raised %v; want 0, raised %v as before", s.priority, s.raised, staleBefore)
		}
		if _, _, err := nudgeModels.Upsert(ctx, in); err != nil {
			t.Fatal(err)
		}
		if s := read(t, stale); s.priority != 1 || !s.raised.After(staleBefore) {
			t.Errorf("raised again, the nudge is at priority %d, raised %v; want 1, raised now", s.priority, s.raised)
		}
	})

	t.Run("a second pass changes nothing more", func(t *testing.T) {
		before := read(t, reraised)
		runNudgePass(ctx)
		if s := read(t, reraised); s.status != before.status || s.priority != before.priority || !s.raised.Equal(before.raised) {
			t.Errorf("a second pass moved the raised-again nudge from %+v to %+v", before, s)
		}
		if s := read(t, stale); s.priority != 1 {
			t.Errorf("a second pass left the nudge raised again at priority %d; want 1: it was raised just now", s.priority)
		}
	})
}
