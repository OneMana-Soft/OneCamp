//go:build integration
// +build integration

package integration_test

// A Slack import keeps a channel's own history as OneCamp's quiet lines (chat
// B28), against real Postgres, Redis and Dgraph: with system messages kept,
// Slack's "joined" (an add, with its inviter), "set the channel topic" and
// "renamed the channel" arrive as the lines OneCamp writes for the same
// changes, as their actor and at their Slack time, where they used to arrive as
// ordinary messages in Slack's own words. A line has no Postgres row, so none
// counts unread, and the import tells nobody. A channel's pins come across
// as their pinner's, at their time.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASlackImportKeepsAChannelsHistoryAsLines -v

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestASlackImportKeepsAChannelsHistoryAsLines(t *testing.T) {
	ctx := context.Background()

	// #launch: Ada adds Bo, sets the topic, asks, and renames the channel.
	var export bytes.Buffer
	zw := zip.NewWriter(&export)
	for name, body := range map[string]string{
		"users.json": `[{"id":"U1","name":"ada","real_name":"Ada","profile":{"email":"ada@acme.test","real_name":"Ada"}},
			{"id":"U2","name":"bo","real_name":"Bo","profile":{"email":"bo@acme.test","real_name":"Bo"}}]`,
		"channels.json": `[{"id":"C1","name":"launch","created":1759300000,"creator":"U1","is_archived":false,"is_general":false,
			"members":["U1","U2"],"topic":{"value":""},"purpose":{"value":""},
			"pins":[{"id":"1759309200.000100","type":"C","created":1759309260,"user":"U2","owner":"U1"}]}]`,
		"launch/2025-10-01.json": `[
			{"type":"message","subtype":"channel_join","user":"U2","inviter":"U1","text":"<@U2> has joined the channel","ts":"1759309100.000050"},
			{"type":"message","subtype":"channel_topic","user":"U1","topic":"Ship on Friday","text":"set the channel topic: Ship on Friday","ts":"1759309150.000060"},
			{"type":"message","user":"U1","text":"Launch on Friday?","ts":"1759309200.000100"},
			{"type":"message","subtype":"channel_name","user":"U1","name":"launch","old_name":"q4-launch","text":"has renamed the channel","ts":"1759309300.000200"},
			{"type":"message","subtype":"channel_purpose","user":"U1","purpose":"Everything for Friday","text":"set the channel purpose: Everything for Friday","ts":"1759309310.000210"},
			{"type":"message","subtype":"pinned_item","user":"U2","item_type":"C","text":"<@U2> pinned a message to this channel.","ts":"1759309320.000220"}]`,
	} {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	e := newRaceEnv(t, map[string][]byte{"raw.zip": export.Bytes()})
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	if _, err := e.PG.Exec(`UPDATE users SET username = 'admin', is_admin = true WHERE id = $1`, e.admin); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": e.admin.String(), "user_name": "admin"})
	// Ada and Bo are already in the workspace, with a phone each and email on,
	// so the import takes them as they are: anything it told them would show.
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	people := map[string]uuid.UUID{}
	for _, name := range []string{"ada", "bo"} {
		id := uuid.New()
		people[name] = id
		for _, q := range []string{
			`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`,
			`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`,
			`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-' || $2)`,
		} {
			if _, err := e.PG.Exec(q, id, name); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_email_id": name + "@acme.test"})
	}
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, e.admin)
	if err != nil || pg == nil {
		t.Fatalf("load the admin: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, e.admin.String())
	if err != nil || gr == nil {
		t.Fatalf("load the admin's node: %v", err)
	}
	admin := userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}

	var mu sync.Mutex
	var pushed []string
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushed = append(pushed, data[firebaseInit.FIREBASE_PUSH_DATA_TYPE])
		mu.Unlock()
	})()

	// The job doesn't say whether to skip system messages, as one started
	// without the plan's options doesn't: the history comes across all the same.
	job := e.job(t, "slack", "Acme", "planned", "slackImport/acme/raw.zip")
	arc, done, err := slackBusiness.OpenStagedZipFromMinIO(ctx, "slackImport/acme/raw.zip")
	if err != nil {
		t.Fatalf("open the export: %v", err)
	}
	if _, err := slackBusiness.BuildPlan(ctx, job, arc); err != nil {
		t.Fatalf("plan: %v", err)
	}
	done()
	slackBusiness.RunImport(ctx, job, &admin)

	var status string
	if err := e.PG.QueryRow(`SELECT status FROM import_jobs WHERE id = $1`, job).Scan(&status); err != nil {
		t.Fatal(err)
	}
	rows, err := e.PG.Query(`SELECT code, message FROM import_errors WHERE import_id = $1`, job)
	if err != nil {
		t.Fatal(err)
	}
	var failures []string
	for rows.Next() {
		var code, msg string
		_ = rows.Scan(&code, &msg)
		failures = append(failures, code+": "+msg)
	}
	rows.Close()

	// One message has a row; the three lines don't (a line is never counted),
	// and Slack's notices OneCamp writes no line for (a purpose set, a pin, which
	// comes across as the pin itself) aren't posts at all.
	var posts int
	if err := e.PG.QueryRow(`SELECT count(*) FROM posts`).Scan(&posts); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Errorf("%d posts in Postgres, want 1, the message (import %s, errors %v)", posts, status, failures)
	}
	// The channel's history in the graph: the lines, as their actor, at their Slack time.
	raw := dg.Query(t, `{ q(func: eq(post_kind, "system")) { post_text post_system post_created_at post_by { user_name } } }`)
	var graph struct {
		Q []struct {
			Text   string    `json:"post_text"`
			System string    `json:"post_system"`
			At     time.Time `json:"post_created_at"`
			By     struct {
				Name string `json:"user_name"`
			} `json:"post_by"`
		} `json:"q"`
	}
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	want := map[string]time.Time{
		"<p>ada added bo.</p>":                                        time.Unix(1759309100, 50000).UTC(),
		"<p>ada set the topic: Ship on Friday</p>":                    time.Unix(1759309150, 60000).UTC(),
		"<p>ada renamed the channel to #launch (was #q4-launch).</p>": time.Unix(1759309300, 200000).UTC(),
	}
	got := map[string]time.Time{}
	for _, p := range graph.Q {
		if p.By.Name != "ada" {
			t.Errorf("a line written as %q: %s", p.By.Name, p.Text)
		}
		got[p.Text] = p.At.UTC()
	}
	if len(got) != len(want) {
		t.Errorf("the lines: %s (import %s, errors %v)", raw, status, failures)
	}
	for text, at := range want {
		if g, ok := got[text]; !ok || g.Sub(at).Abs() > time.Millisecond {
			t.Errorf("line %q: at %v, want %v (got %v)", text, g, at, got)
		}
	}
	// Bo's pin of Ada's question comes across as his, at his time (pin_pass.go).
	var everyPost struct {
		Q []struct {
			UUID string `json:"post_uuid"`
			Text string `json:"post_text"`
		} `json:"q"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ q(func: has(post_uuid)) { post_uuid post_text } }`), &everyPost); err != nil {
		t.Fatal(err)
	}
	question := ""
	for _, p := range everyPost.Q {
		if strings.Contains(p.Text, "Launch on Friday?") {
			question = p.UUID
		}
	}
	var pinnedBy uuid.UUID
	var pinnedAt time.Time
	if err := e.PG.QueryRow(`SELECT pinned_by, pinned_at FROM message_pins WHERE conversation_kind = 'channel' AND message_id = $1`, question).Scan(&pinnedBy, &pinnedAt); err != nil {
		t.Errorf("the pin: %v (import %s, errors %v)", err, status, failures)
	} else if pinnedBy != people["bo"] || !pinnedAt.Equal(time.Unix(1759309260, 0)) {
		t.Errorf("pinned by %s at %v, want Bo (%s) at %v", pinnedBy, pinnedAt, people["bo"], time.Unix(1759309260, 0).UTC())
	}

	for _, f := range failures {
		if strings.Contains(f, "MESSAGE_IMPORT_FAILED") {
			t.Errorf("the import logged a failed message: %s", f)
		}
	}

	var emails int
	if err := e.PG.QueryRow(`SELECT count(*) FROM notification_email_queue`).Scan(&emails); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pushed) != 0 || emails != 0 {
		t.Errorf("the import told people of what it brought: pushes %v, %d emails", pushed, emails)
	}
}
