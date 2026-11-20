//go:build integration

package business

// GET /calls/linkable (LinkableCalls): the calls a table's picker offers are
// the reader's own (the calls of the conversations they're in, as the call
// rule lets them read), newest first, capped, filtered by their title; read
// in one statement a page, through the conversations index and
// call_sessions_resource_idx (model.SessionsBefore).

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestLinkableCallsAreTheReadersOwnNewestFirst(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Maya is in #leadership (private) and #general; Bo is in #general and
	// #board (private), which Maya isn't.
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"maya", "bo"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	channels := map[string]string{}
	for _, c := range []struct {
		name    string
		private bool
		members []string
	}{{"leadership", true, []string{"maya"}}, {"general", false, []string{"maya", "bo"}}, {"board", true, []string{"bo"}}} {
		id := uuid.New()
		channels[c.name] = id.String()
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, $3)`, id, c.name, c.private); err != nil {
			t.Fatal(err)
		}
		var members []map[string]any
		var ids []string
		for _, m := range c.members {
			members = append(members, map[string]any{"uid": "_:" + m})
			ids = append(ids, people[m].String())
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + c.name, "dgraph.type": "Channel", "ch_uuid": id.String(), "ch_name": c.name,
			"ch_private": c.private, "ch_deleted_at": "0001-01-01T00:00:00Z", "ch_members": members})
		// The conversations index, as a channel's making records it.
		conversationDomain.Opened(ctx, conversationDomain.KindChannel, id.String(), ids[0], ids, nil)
	}
	dg.Mutate(t, nodes)

	now := time.Now().UTC().Truncate(time.Second)
	call := func(kind, resource, topic string, at time.Time) string {
		t.Helper()
		end := at.Add(30 * time.Minute)
		room := resource
		if kind == "meeting" {
			room = "meet-" + resource
		}
		s := model.Session{ID: uuid.New(), Room: room, Kind: model.KindHuddle, ResourceKind: kind, ResourceID: resource, Topic: topic,
			StartedAt: at, EndedAt: &end, EndedReason: model.EndedLastLeft}
		if err := model.KeepEndedSession(ctx, s, nil, nil); err != nil {
			t.Fatalf("keep a call in %s: %v", resource, err)
		}
		if _, err := env.PG.Exec(`UPDATE call_sessions SET topic = NULLIF($2, '') WHERE id = $1`, s.ID, topic); err != nil {
			t.Fatal(err)
		}
		return s.ID.String()
	}
	day := func(at time.Time) string { return at.Format("2 Jan 2006") }
	reorg := call("channel", channels["leadership"], "Reorg sync", now.Add(-1*time.Hour))
	standup := call("channel", channels["general"], "Standup", now.Add(-2*time.Hour))
	quiet := call("channel", channels["leadership"], "", now.Add(-3*time.Hour))
	weekly := call("channel", channels["general"], "", now.Add(-4*time.Hour))
	secret := call("channel", channels["board"], "Board prep", now.Add(-30*time.Minute))
	call("meeting", uuid.NewString(), "Vendor call", now.Add(-10*time.Minute))
	breakout := call("channel", channels["general"], "Room 2", now.Add(-90*time.Minute))
	if _, err := env.PG.Exec(`UPDATE call_sessions SET kind = 'breakout', parent_session_id = $2 WHERE id = $1`, breakout, standup); err != nil {
		t.Fatal(err)
	}

	as := func(name string) *userModels.UserInfo {
		return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: people[name].String()}}
	}
	ids := func(calls []LinkableCall) []string {
		out := []string{}
		for _, c := range calls {
			out = append(out, c.ID)
		}
		return out
	}
	reads := 0
	before := sessionsBefore
	sessionsBefore = func(ctx context.Context, reader uuid.UUID, after model.SessionsAfter, size int) ([]model.Session, model.SessionsAfter, error) {
		reads++
		return before(ctx, reader, after, size)
	}
	t.Cleanup(func() { sessionsBefore = before })

	// Maya: her two channels' calls, newest first; never #board's (she isn't
	// in it), an instant meeting's or a breakout; one read for the page.
	got, err := LinkableCalls(ctx, as("maya"), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{reorg, standup, quiet, weekly}; !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("Maya's calls %v, want %v", ids(got), want)
	}
	if reads != 1 {
		t.Errorf("Maya's calls took %d reads of calls, want 1", reads)
	}
	if got[2].Title != "Call in #leadership, "+day(now.Add(-3*time.Hour)) || got[0].Title != "Reorg sync" {
		t.Errorf("titles %q, %q: want the topic, else the channel and the day", got[0].Title, got[2].Title)
	}

	// Bo: #board's and #general's, never #leadership's.
	if got, err = LinkableCalls(ctx, as("bo"), "", 0); err != nil {
		t.Fatal(err)
	}
	if want := []string{secret, standup, weekly}; !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("Bo's calls %v, want %v", ids(got), want)
	}

	// Capped, and the next of them not left out: two, then by title.
	if got, err = LinkableCalls(ctx, as("maya"), "", 2); err != nil {
		t.Fatal(err)
	}
	if want := []string{reorg, standup}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("Maya's two newest %v, want %v", ids(got), want)
	}
	if got, err = LinkableCalls(ctx, as("maya"), "#general", 0); err != nil {
		t.Fatal(err)
	}
	if want := []string{weekly}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf(`Maya's calls titled "#general" %v, want %v`, ids(got), want)
	}

	// Someone in no conversation reads none; a bot is refused.
	stranger := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: uuid.NewString()}}
	if got, err = LinkableCalls(ctx, stranger, "", 0); err != nil || len(got) != 0 {
		t.Errorf("a stranger's calls %v (%v), want none", ids(got), err)
	}
	bot := as("bo")
	bot.UserDgraphInfo.IsBot = true
	if _, err = LinkableCalls(ctx, bot, "", 0); !errors.Is(err, ErrLinkableCallsRefused) {
		t.Errorf("a bot's listing: %v, want refused", err)
	}
	// Nor is a listing through an API token, even a member's (CR-375 N5).
	if _, err = LinkableCalls(helpers.WithActsThroughToken(ctx), as("maya"), "", 0); !errors.Is(err, ErrLinkableCallsRefused) {
		t.Errorf("a listing through an API token: %v, want refused", err)
	}
}
