//go:build integration

package business

// model.SessionsBefore past its first page (CR-375 F3): pages of 2 over the
// calls of two conversations, one holding more than a page (so a
// conversation's edge is its size-th newest call, not -infinity), two calls
// that started at the same instant (so the cursor's id decides between them)
// and a breakout (its call's, never a call of its own). Put together, the
// pages are every call of the reader's conversations, newest first by
// (started_at, id), each once; a conversation the reader isn't in gives none.

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestSessionsBeforePagesAddUpToTheNewestFirstOrder(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)

	maya, bo := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"maya": maya, "bo": bo} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	// Maya is in #alpha and #beta; #gamma is Bo's alone.
	channels := map[string]string{}
	for name, members := range map[string][]string{"alpha": {maya.String(), bo.String()}, "beta": {maya.String()}, "gamma": {bo.String()}} {
		id := uuid.New()
		channels[name] = id.String()
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, id, name); err != nil {
			t.Fatal(err)
		}
		conversationDomain.Opened(ctx, conversationDomain.KindChannel, id.String(), members[0], members, nil)
	}

	now := time.Now().UTC().Truncate(time.Second)
	type kept struct {
		id uuid.UUID
		at time.Time
	}
	var want []kept
	call := func(channel string, at time.Time) uuid.UUID {
		t.Helper()
		end := at.Add(20 * time.Minute)
		s := model.Session{ID: uuid.New(), Room: channels[channel], Kind: model.KindHuddle, ResourceKind: "channel", ResourceID: channels[channel],
			StartedAt: at, EndedAt: &end, EndedReason: model.EndedLastLeft}
		if err := model.KeepEndedSession(ctx, s, nil, nil); err != nil {
			t.Fatalf("keep a call in #%s: %v", channel, err)
		}
		if channel != "gamma" {
			want = append(want, kept{s.ID, at})
		}
		return s.ID
	}
	first := call("alpha", now.Add(-1*time.Hour))
	call("alpha", now.Add(-2*time.Hour))
	call("beta", now.Add(-2*time.Hour)) // the same instant as alpha's second
	call("beta", now.Add(-3*time.Hour))
	call("alpha", now.Add(-4*time.Hour))
	call("beta", now.Add(-5*time.Hour))
	call("alpha", now.Add(-6*time.Hour))
	call("gamma", now.Add(-30*time.Minute))
	breakout := uuid.New()
	end := now.Add(-80 * time.Minute)
	room := model.Session{ID: breakout, Room: channels["alpha"] + "-room-2", Kind: model.KindHuddle, ResourceKind: "channel", ResourceID: channels["alpha"],
		StartedAt: now.Add(-90 * time.Minute), EndedAt: &end, EndedReason: model.EndedLastLeft}
	if err := model.KeepEndedSession(ctx, room, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET kind = 'breakout', parent_session_id = $2 WHERE id = $1`, breakout, first); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool {
		if !want[i].at.Equal(want[j].at) {
			return want[i].at.After(want[j].at)
		}
		return want[i].id.String() > want[j].id.String()
	})
	var wantIDs []uuid.UUID
	for _, k := range want {
		wantIDs = append(wantIDs, k.id)
	}

	var got []uuid.UUID
	after := model.SessionsAfter{}
	for page := 0; ; page++ {
		if page > len(want) {
			t.Fatalf("still reading after %d pages: %v", page, got)
		}
		rows, next, err := model.SessionsBefore(ctx, maya, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 2 {
			t.Fatalf("page %d holds %d calls, more than its 2", page, len(rows))
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			got = append(got, r.ID)
		}
		after = next
	}
	if !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("the pages put together:\n got %v\nwant %v (newest first by start and id; no breakout %s, nothing of #gamma)", got, wantIDs, breakout)
	}
}
