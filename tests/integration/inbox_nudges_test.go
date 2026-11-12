//go:build integration
// +build integration

package integration_test

// The owner's Inbox badge stayed at 1 (V-577, 5 Nov): the web added the open
// urgent OneCamp AI nudges to the server's count, outside the one unread rule,
// so a five-month-old "Overdue commitment" kept the badge up after every Inbox
// open, with nothing on All to say why. The server counts them now, by the rule
// it counts everything by: an urgent nudge raised since the person's last look
// at their Inbox (made, or raised again) is unread. Opening the Inbox clears it
// from the badge; it stays listed until it is resolved or dismissed. The list
// marks each nudge's dot by the same rule, and the engine's pass finding an
// open nudge's signal again raises nothing.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheInboxBadgeCountsUrgentNudgesByTheOneRule -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	_ "github.com/akashc777/OneCamp/business/Follow" // hands the badge its notices, as in the app
	readStateBusiness "github.com/akashc777/OneCamp/business/ReadState"
	aiController "github.com/akashc777/OneCamp/controllers/AI"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	nudgeModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheInboxBadgeCountsUrgentNudgesByTheOneRule(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// OneCamp AI on: the Inbox lists nudges only then.
	ollama := httptest.NewServer(fakeOllama{})
	defer ollama.Close()
	t.Setenv("OLLAMA_HOST", ollama.URL)
	if _, err := env.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1`, fakeEmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil {
		t.Fatalf("init AI: %v", err)
	}
	if !ai.GetService().IsEnabled() {
		t.Fatal("AI did not come up enabled; nothing below would run")
	}
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})

	owner, maya := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"owner": owner, "maya": maya} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	// The owner last looked at their Inbox an hour ago.
	if _, err := env.PG.Exec(`INSERT INTO last_seen_activity (user_id, user_last_seen) VALUES ($1, NOW() - interval '1 hour')`, owner); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "owner"},
	})
	who := userModels.UserInfo{}
	who.UserPostgresInfo.Id = owner
	who.UserDgraphInfo.Uuid, who.UserDgraphInfo.Uid = owner.String(), uids["owner"]

	// nudge writes an overdue commitment's nudge for whom, raised ago.
	nudge := func(t *testing.T, whom uuid.UUID, key, status string, priority int, ago time.Duration) string {
		t.Helper()
		var id string
		if err := env.PG.QueryRow(`INSERT INTO workspace_nudges
			(user_id, kind, title, body, source_type, source_id, status, priority, dedup_key, created_at, updated_at)
			VALUES ($1, 'overdue_commitment', 'Overdue commitment', $7, 'memory', $3, $4, $5, $2, $6, $6)
			RETURNING id`, whom, key, uuid.NewString(), status, priority, time.Now().Add(-ago), "You committed to: "+key+".").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	badge := func(t *testing.T) uint64 {
		t.Helper()
		n, err := activityBusiness.UnreadActivityCount(ctx, owner, uids["owner"])
		if err != nil {
			t.Fatalf("counting the badge: %v", err)
		}
		return n
	}
	// listed is GET /ai/nudges as the owner: each listed nudge's id and its dot.
	listed := func(t *testing.T) map[string]bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/ai/nudges", nil).WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, who))
		rec := httptest.NewRecorder()
		aiController.ListNudges(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /ai/nudges answered %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data struct {
				Nudges []struct {
					ID     string `json:"id"`
					Unread bool   `json:"unread"`
				} `json:"nudges"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, n := range body.Data.Nudges {
			out[n.ID] = n.Unread
		}
		return out
	}
	// raisedAt is when the nudge was last raised, as the badge reads it.
	raisedAt := func(t *testing.T, id string) time.Time {
		t.Helper()
		var at time.Time
		if err := env.PG.QueryRow(`SELECT updated_at FROM workspace_nudges WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}

	old := nudge(t, owner, "memory:old", "open", 1, 2*time.Hour)
	t.Run("an urgent nudge raised before the last look counts nothing", func(t *testing.T) {
		if got := badge(t); got != 0 {
			t.Errorf("the badge says %d; want 0: the owner looked at their Inbox after it was raised", got)
		}
		if dot, ok := listed(t)[old]; !ok || dot {
			t.Errorf("the list has it %v with a dot %v; want it listed, without a dot", ok, dot)
		}
	})

	fresh := nudge(t, owner, "memory:fresh", "open", 1, 10*time.Minute)
	t.Run("one raised since counts, and the list marks it", func(t *testing.T) {
		if got := badge(t); got != 1 {
			t.Errorf("the badge says %d; want 1: an urgent nudge raised since the last look is unread, like anything else in the Inbox", got)
		}
		l := listed(t)
		if !l[fresh] || l[old] {
			t.Errorf("the list marks %v; want a dot on the fresh nudge alone", l)
		}
	})

	calm := nudge(t, owner, "memory:calm", "open", 0, 5*time.Minute)
	nudge(t, maya, "memory:mayas", "open", 1, 5*time.Minute)
	nudge(t, owner, "memory:dismissed", "dismissed", 1, 5*time.Minute)
	t.Run("one that isn't urgent, someone else's, or a closed one counts nothing", func(t *testing.T) {
		if got := badge(t); got != 1 {
			t.Errorf("the badge says %d; want 1: a nudge at priority 0, maya's, and a dismissed one never count", got)
		}
		l := listed(t)
		if dot, ok := l[calm]; !ok || dot {
			t.Errorf("the calm nudge is listed %v with a dot %v; want it listed, without a dot: the badge doesn't count it", ok, dot)
		}
		if len(l) != 3 {
			t.Errorf("the owner's list has %d nudges %v; want their three open ones, never maya's or a dismissed one", len(l), l)
		}
	})

	t.Run("opening the Inbox clears it from the badge, and it stays listed", func(t *testing.T) {
		n, err := readStateBusiness.InboxSeen(ctx, &who)
		if err != nil || n == nil || *n != 0 {
			t.Fatalf("opening the Inbox answered %v, %v; want 0", n, err)
		}
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("still at work: %v", busy)
		}
		if got := badge(t); got != 0 {
			t.Errorf("after opening the Inbox the badge says %d; want 0", got)
		}
		l := listed(t)
		if dot, ok := l[fresh]; !ok || dot {
			t.Errorf("after opening the Inbox the fresh nudge is listed %v with a dot %v; want it listed until it is resolved or dismissed, without a dot", ok, dot)
		}
	})

	// The engine's pass (every half hour while nudges are on) writes each
	// signal that still holds through UpsertMany. Finding the same overdue
	// commitment open again is no news: it moved updated_at, so the badge
	// came back after every pass.
	t.Run("an engine pass over the same overdue item raises nothing", func(t *testing.T) {
		before := raisedAt(t, fresh)
		if _, err := nudgeModels.UpsertMany(ctx, owner, []nudgeModels.UpsertInput{{
			UserID: owner, Kind: nudgeModels.KindOverdueCommitment, Title: "Overdue commitment",
			Body: "You committed to: memory:fresh (was due Jun 11).", SourceType: "memory", Priority: 1, DedupKey: "memory:fresh",
		}}); err != nil {
			t.Fatal(err)
		}
		if after := raisedAt(t, fresh); !after.Equal(before) {
			t.Errorf("an engine pass moved the open nudge's updated_at from %v to %v; want it kept: it was already open", before, after)
		}
		if got := badge(t); got != 0 {
			t.Errorf("after an engine pass the badge says %d; want 0", got)
		}
	})

	t.Run("re-opened by the engine, it counts again", func(t *testing.T) {
		if err := nudgeModels.SetStatus(ctx, uuid.MustParse(old), owner, nudgeModels.StatusDismissed); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		up, err := nudgeModels.UpsertMany(ctx, owner, []nudgeModels.UpsertInput{{
			UserID: owner, Kind: nudgeModels.KindOverdueCommitment, Title: "Overdue commitment",
			Body: "You committed to: memory:old.", SourceType: "memory", Priority: 1, DedupKey: "memory:old",
		}})
		if err != nil || len(up) != 1 || !up[0].Inserted {
			t.Fatalf("re-opening wrote %+v (%v); want one new open nudge", up, err)
		}
		if got := badge(t); got != 1 {
			t.Errorf("a nudge re-opened since the last look makes the badge %d; want 1", got)
		}
		if l := listed(t); !l[up[0].ID.String()] {
			t.Errorf("the list marks %v; want a dot on the re-opened nudge", l)
		}
	})

	t.Run("raised again, it counts again", func(t *testing.T) {
		time.Sleep(5 * time.Millisecond)
		if _, _, err := nudgeModels.Upsert(ctx, nudgeModels.UpsertInput{
			UserID: owner, Kind: nudgeModels.KindOverdueCommitment, Title: "Overdue commitment",
			Body: "You committed to: memory:fresh, and it slipped again.", SourceType: "memory", Priority: 1, DedupKey: "memory:fresh",
		}); err != nil {
			t.Fatal(err)
		}
		if got := badge(t); got != 2 {
			t.Errorf("a nudge raised again since the last look makes the badge %d; want 2, with the re-opened one", got)
		}
		if l := listed(t); !l[fresh] {
			t.Errorf("the list marks %v; want a dot on the nudge raised again", l)
		}
	})

	t.Run("with OneCamp AI off nothing lists nudges, and the badge counts none", func(t *testing.T) {
		if _, err := env.PG.Exec(`UPDATE ai_settings SET enabled = false`); err != nil {
			t.Fatal(err)
		}
		if err := ai.ReloadAIService(ctx); err != nil {
			t.Fatal(err)
		}
		if ai.GetService().IsEnabled() {
			t.Fatal("AI still on")
		}
		if got := badge(t); got != 0 {
			t.Errorf("with AI off the badge says %d; want 0: the Inbox shows no nudges then", got)
		}
	})
}

// Dismissing or acting on a nudge names it on the person's own activity
// topic, so their other tabs drop that nudge, not just a count (V-577, the
// notification audit's H2).
func TestADismissedNudgeIsNamedLive(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	broker := integration.RecordMqtt(t)
	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'owner', 'owner', NOW(), NOW())`, owner, owner.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	who := userModels.UserInfo{}
	who.UserPostgresInfo.Id = owner
	who.UserDgraphInfo.Uuid = owner.String()
	open := func(t *testing.T, key string) string {
		t.Helper()
		var id string
		if err := env.PG.QueryRow(`INSERT INTO workspace_nudges (user_id, kind, title, body, status, priority, dedup_key)
			VALUES ($1, 'overdue_commitment', 'Overdue commitment', 'You committed to it.', 'open', 1, $2) RETURNING id`, owner, key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	dismissed, acted := open(t, "k-dismiss"), open(t, "k-act")
	open(t, "k-stays")
	call := func(t *testing.T, h http.HandlerFunc, id string) {
		t.Helper()
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id)
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, who), chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/ai/nudges/"+id, nil).WithContext(c))
		if rec.Code != http.StatusOK {
			t.Fatalf("answered %d: %s", rec.Code, rec.Body.String())
		}
	}
	call(t, aiController.DismissNudge, dismissed)
	call(t, aiController.ActNudge, acted)

	type told struct {
		Action    string   `json:"action"`
		NudgeIDs  []string `json:"nudge_ids"`
		OpenCount int      `json:"open_count"`
	}
	var got []told
	for _, raw := range broker.Messages(helpers.GetMqttTopicForUserActivity(owner.String()), mqttStruct.MESSAGE_AI_NUDGE) {
		var m told
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	same := func(a, b told) bool {
		return a.Action == b.Action && slices.Equal(a.NudgeIDs, b.NudgeIDs) && a.OpenCount == b.OpenCount
	}
	want := []told{{"cleared", []string{dismissed}, 2}, {"cleared", []string{acted}, 1}}
	if len(got) != 2 || !same(got[0], want[0]) || !same(got[1], want[1]) {
		t.Errorf("their topic was told %+v; want %+v: each closed nudge by its id, with the open count after", got, want)
	}
}
