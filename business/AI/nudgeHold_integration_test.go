//go:build integration

package business

// V-615: Dismiss was a 3-day hold, not an end. The engine skipped a signal
// only while someone had dismissed or acted on it within the last 3 days, so
// a dismissed open question came back every 3 days for as long as it stayed
// open, with no bound (an overdue commitment stops at 14 days overdue).
//
// Now ONE rule holds every signal someone handled: it stays down for at least
// 3 days and after that until its item changes. A question is nudged of only
// while it is stale for at most the commitments' 14 days, so the 30-day purge
// of handled nudges never lets a held signal back. Held signals no longer take
// the per-pass places of new ones.
//
// Run: go test -tags=integration ./business/AI/ -run TestADismissedNudgeStaysDownUntilItsItemChanges -v

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestADismissedNudgeStaysDownUntilItsItemChanges(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)

	// Two members, each in their own channel: one for the holds, one with a
	// full pass of held signals.
	owner, ownerChannel := nudgeMember(t, env, dg, "owner")
	busy, busyChannel := nudgeMember(t, env, dg, "busy")
	item := func(who, channel uuid.UUID, kind, content, untouched, due string) uuid.UUID {
		return memoryItemFor(t, env, who, channel, kind, content, untouched, due)
	}
	handled := func(who, id uuid.UUID, kind, status, ago string) { nudgeRow(t, env, who, id, kind, status, ago) }
	// pass runs the engine's pass and returns the items of the member's open
	// nudges.
	pass := func(who uuid.UUID) []string {
		t.Helper()
		raiseNudges(ctx, time.Now())
		rows, err := env.PG.Query(`SELECT source_id FROM workspace_nudges WHERE user_id = $1 AND status = 'open'`, who)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var open []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			open = append(open, s)
		}
		return open
	}

	t.Run("a handled question stays down until it changes, and a question is nudged of only while recently stale", func(t *testing.T) {
		q := func(content, untouched string) uuid.UUID {
			return item(owner, ownerChannel, "question", content, untouched, "NULL")
		}
		dismissed4d := q("Who signs off the venue?", "10 days")
		handled(owner, dismissed4d, "stale_question", "dismissed", "4 days")
		acted4d := q("Which printer do we use?", "10 days")
		handled(owner, acted4d, "stale_question", "acted", "4 days")
		dismissedHourAgo := q("Is the deck final?", "10 days")
		handled(owner, dismissedHourAgo, "stale_question", "dismissed", "1 hour")
		changedSince := q("When does the launch post go out?", "8 days")
		handled(owner, changedSince, "stale_question", "dismissed", "9 days")
		stale20d := q("Who owns the press list?", "20 days")
		stale25d := q("Xnnnxxmmx?", "25 days")

		open := pass(owner)
		for name, c := range map[string]struct {
			id   uuid.UUID
			want bool
		}{
			"dismissed 4 days ago, unchanged since":          {dismissed4d, false},
			"acted on 4 days ago, unchanged since":           {acted4d, false},
			"dismissed an hour ago":                          {dismissedHourAgo, false},
			"changed after it was dismissed":                 {changedSince, true},
			"untouched 20 days (stale 13 days), not handled": {stale20d, true},
			"untouched 25 days (stale 18 days), not handled": {stale25d, false},
		} {
			if got := slices.Contains(open, c.id.String()); got != c.want {
				t.Errorf("the question %s: nudged %v; want %v", name, got, c.want)
			}
		}
	})

	t.Run("held signals don't take the places of new ones", func(t *testing.T) {
		// A full pass (nudgeMaxPerUser) of overdue commitments, all dismissed
		// an hour ago, outranks the one new stale question.
		for i := 1; i <= nudgeMaxPerUser; i++ {
			id := item(busy, busyChannel, "commitment", fmt.Sprintf("Commitment %d", i), "30 days",
				fmt.Sprintf("NOW() - interval '%d days'", i))
			handled(busy, id, "overdue_commitment", "dismissed", "1 hour")
		}
		fresh := item(busy, busyChannel, "question", "Who books the photographer?", "10 days", "NULL")

		open := pass(busy)
		if !slices.Equal(open, []string{fresh.String()}) {
			t.Errorf("the busy member's open nudges are %q; want only the new question %s", open, fresh)
		}
	})
}

// nudgeMember writes a member in a channel of their own, in Postgres and the
// graph, and returns both ids.
func nudgeMember(t *testing.T, env *integration.Env, dg *integration.DgraphEnv, name string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	zero := "0001-01-01T00:00:00Z"
	id, channel := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
		"user_channels": []map[string]any{{"uid": "_:ch" + name, "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": name, "ch_deleted_at": zero}}}})
	return id, channel
}

// memoryItemFor writes who's open memory item in channel: kind, last touched
// `untouched` ago (SQL interval), due `due` (SQL, commitments only).
func memoryItemFor(t *testing.T, env *integration.Env, who, channel uuid.UUID, kind, content, untouched, due string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := env.PG.QueryRow(`INSERT INTO workspace_memory_items
		(kind, content, status, owner_user_id, channel_uuid, due_at, dedup_hash, created_at, updated_at)
		VALUES ($1, $2, 'open', $3, $4, `+due+`, $5, NOW() - $6::interval, NOW() - $6::interval)
		RETURNING id`, kind, content, who, channel, uuid.NewString(), untouched).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// nudgeRow writes who's nudge of kind for the item as `status`, last changed
// `ago` (SQL interval), and returns its id: an open one, or what Dismiss (or
// its CTA) leaves behind.
func nudgeRow(t *testing.T, env *integration.Env, who, item uuid.UUID, kind, status, ago string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := env.PG.QueryRow(`INSERT INTO workspace_nudges
		(user_id, kind, title, source_type, source_id, dedup_key, status, created_at, updated_at)
		VALUES ($1, $2, 'Open question needs attention', 'memory', $3, $4, $5, NOW() - $6::interval, NOW() - $6::interval)
		RETURNING id`, who, kind, item.String(), "memory:"+item.String(), status, ago).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
