//go:build integration

package integration_test

// The call's post (calls CALLS-3b): a session takes one post, once, whoever
// asks first; a live call's post is found by it, an ended one's isn't; the
// index that finds it goes down and up again.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	chatDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	postDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
)

func callPostsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_call_posts."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one call_posts %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCallATakesOnePostOnce(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	session, room := uuid.New(), uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, $2, 'huddle', 'channel', $2, NOW())`, session, room); err != nil {
		t.Fatal(err)
	}
	s, err := callStateBusiness.Get(ctx, session.String())
	if err != nil || s == nil {
		t.Fatalf("the session: %+v, %v", s, err)
	}
	first, second := uuid.New(), uuid.New()
	if won, err := callStateBusiness.AttachPost(ctx, s, first); err != nil || !won {
		t.Fatalf("the first post: %v, %v", won, err)
	}
	if won, err := callStateBusiness.AttachPost(ctx, s, second); err != nil || won {
		t.Errorf("a second post took the session: %v, %v", won, err)
	}
	if again, _ := callStateBusiness.Get(ctx, session.String()); again == nil || again.PostUUID != first.String() {
		t.Errorf("the session's post: %+v", again)
	}
	live, err := callStateModel.LiveCallsOfPosts(ctx, []uuid.UUID{first, second})
	if err != nil || live[first] != room || len(live) != 1 {
		t.Errorf("the live call by its post: %v, %v", live, err)
	}
	// Its post isn't deleted while the call goes on (the delete routes ask).
	if on, err := callStateBusiness.IsLivePost(ctx, first); err != nil || !on {
		t.Errorf("the call's post while it goes on: %v, %v", on, err)
	}
	if on, _ := callStateBusiness.IsLivePost(ctx, second); on {
		t.Error("a post that isn't the call's")
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET ended_at = NOW() WHERE id = $1`, session); err != nil {
		t.Fatal(err)
	}
	if live, _ := callStateModel.LiveCallsOfPosts(ctx, []uuid.UUID{first}); len(live) != 0 {
		t.Errorf("an ended call by its post: %v", live)
	}
	if on, _ := callStateBusiness.IsLivePost(ctx, first); on {
		t.Error("an ended call's post still held")
	}

	if _, err := env.PG.Exec(callPostsMigration(t, "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := env.PG.Exec(callPostsMigration(t, "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	var indexed bool
	if err := env.PG.QueryRow(`SELECT to_regclass('call_sessions_live_post') IS NOT NULL`).Scan(&indexed); err != nil || !indexed {
		t.Errorf("the live post index: %v, %v", indexed, err)
	}
}

// The repair finds a live conversation's call still without its post a while
// after it started; a failed try counts, a refusal passes it over for good,
// and a call that has its post, or ended, is never asked about.
func TestCallPostRepairFindsWhatsMissing(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	missing, young, posted, ended := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, s := range []struct {
		id    uuid.UUID
		age   string
		post  any
		ended any
	}{{missing, "2 minutes", nil, nil}, {young, "5 seconds", nil, nil}, {posted, "2 minutes", uuid.New(), nil}, {ended, "2 minutes", nil, "now"}} {
		q := `INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at, post_uuid, ended_at)
			VALUES ($1, $2, 'huddle', 'channel', $2, NOW() - $3::interval, $4, CASE WHEN $5::text IS NULL THEN NULL ELSE NOW() END)`
		if _, err := env.PG.Exec(q, s.id, uuid.NewString(), s.age, s.post, s.ended); err != nil {
			t.Fatal(err)
		}
	}
	find := func() []uuid.UUID {
		ids, err := callStateModel.PostlessHuddles(ctx, time.Now().Add(-30*time.Second), 5, 50)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got := find(); len(got) != 1 || got[0] != missing {
		t.Fatalf("found %v, want only %s", got, missing)
	}
	if err := callStateModel.NotePostTry(ctx, missing, false); err != nil {
		t.Fatal(err)
	}
	if got := find(); len(got) != 1 {
		t.Errorf("a failed try passed it over: %v", got)
	}
	if err := callStateModel.NotePostTry(ctx, missing, true); err != nil {
		t.Fatal(err)
	}
	if got := find(); len(got) != 0 {
		t.Errorf("a refused call is still asked about: %v", got)
	}
}

// A call's end writes its facts on the call's post alone: an ordinary
// message, or a system line, asked by its uuid, keeps its own (CHAT-LEAD-2's
// hardening of SetCallFactsByUUID).
func TestCallFactsAreWrittenOnTheCallsPostAlone(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	ordinary, huddle, line, chat, chatHuddle := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:o", "dgraph.type": "Post", "post_uuid": ordinary, "post_text": "<p>hi</p>"},
		{"uid": "_:h", "dgraph.type": "Post", "post_uuid": huddle, "post_kind": "huddle", "post_system": `{"session":"s"}`},
		{"uid": "_:l", "dgraph.type": "Post", "post_uuid": line, "post_kind": "system", "post_system": `{"line":"joined"}`},
		{"uid": "_:c", "dgraph.type": "Chat", "chat_uuid": chat},
		{"uid": "_:ch", "dgraph.type": "Chat", "chat_uuid": chatHuddle, "chat_kind": "huddle"},
	})
	facts := `{"session":"s","seconds":60}`
	for _, id := range []string{ordinary, huddle, line} {
		if err := postDgraphModels.SetCallFactsByUUID(ctx, id, facts); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{chat, chatHuddle} {
		if err := chatDgraphModels.SetCallFactsByUUID(ctx, id, facts); err != nil {
			t.Fatal(err)
		}
	}
	var got struct {
		P []struct {
			ID     string `json:"post_uuid"`
			System string `json:"post_system"`
		} `json:"p"`
		C []struct {
			ID     string `json:"chat_uuid"`
			System string `json:"chat_system"`
		} `json:"c"`
	}
	q := fmt.Sprintf(`{ p(func: eq(post_uuid, [%q, %q, %q])) { post_uuid post_system } c(func: eq(chat_uuid, [%q, %q])) { chat_uuid chat_system } }`, ordinary, huddle, line, chat, chatHuddle)
	if err := json.Unmarshal(dg.Query(t, q), &got); err != nil || len(got.P) != 3 || len(got.C) != 2 {
		t.Fatalf("read back %+v, %v", got, err)
	}
	want := map[string]string{ordinary: "", huddle: facts, line: `{"line":"joined"}`, chat: "", chatHuddle: facts}
	for _, p := range got.P {
		if p.System != want[p.ID] {
			t.Errorf("post %s has %q, want %q", p.ID, p.System, want[p.ID])
		}
	}
	for _, c := range got.C {
		if c.System != want[c.ID] {
			t.Errorf("message %s has %q, want %q", c.ID, c.System, want[c.ID])
		}
	}
}
