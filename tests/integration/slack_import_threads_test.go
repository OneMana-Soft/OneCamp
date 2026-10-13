//go:build integration
// +build integration

package integration_test

// A Slack import brings a channel's threads across, against real Postgres,
// Redis and Dgraph: a reply lands under the message it answers.
//
// Every channel thread reply was skipped. The import finds a reply's message
// in the graph with a query that named a variable it never declared, which
// the graph refuses, so each reply was logged as failed and the import still
// said it was done. Its edits and deletes went to Postgres only, the same way.
//
// And the replies it brings tell nobody: years of a team's threads are not
// news to push or email to anyone.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASlackImportBringsThreadsAcross -v

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

func TestASlackImportBringsThreadsAcross(t *testing.T) {
	ctx := context.Background()

	// #launch: Ada asks, Bo answers in the thread, then Ada edits her question.
	var export bytes.Buffer
	zw := zip.NewWriter(&export)
	for name, body := range map[string]string{
		"users.json": `[{"id":"U1","name":"ada","real_name":"Ada","profile":{"email":"ada@acme.test","real_name":"Ada"}},
			{"id":"U2","name":"bo","real_name":"Bo","profile":{"email":"bo@acme.test","real_name":"Bo"}}]`,
		"channels.json": `[{"id":"C1","name":"launch","created":1759300000,"creator":"U1","is_archived":false,"is_general":false,
			"members":["U1","U2"],"topic":{"value":""},"purpose":{"value":""}}]`,
		"launch/2025-10-01.json": `[
			{"type":"message","user":"U1","text":"Launch on Friday?","ts":"1759309200.000100","thread_ts":"1759309200.000100","reply_count":1},
			{"type":"message","user":"U2","text":"Yes, Friday it is","ts":"1759309260.000200","thread_ts":"1759309200.000100","parent_user_id":"U1"}]`,
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
	for _, name := range []string{"ada", "bo"} {
		id := uuid.New()
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

	var replies int
	if err := e.PG.QueryRow(`SELECT count(*) FROM comments`).Scan(&replies); err != nil {
		t.Fatal(err)
	}
	if replies != 1 {
		t.Errorf("%d replies in Postgres, want 1 (import %s, errors %v)", replies, status, failures)
	}
	// The reply is under the message it answers, in the graph.
	raw := dg.Query(t, `{ q(func: has(post_uuid)) { post_text post_comments { comment_text } } }`)
	var graph struct {
		Q []struct {
			Text     string `json:"post_text"`
			Comments []struct {
				Text string `json:"comment_text"`
			} `json:"post_comments"`
		} `json:"q"`
	}
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	answered := false
	for _, p := range graph.Q {
		if strings.Contains(p.Text, "Launch on Friday?") {
			for _, c := range p.Comments {
				answered = answered || strings.Contains(c.Text, "Yes, Friday it is")
			}
		}
	}
	if !answered {
		t.Errorf("the thread in the graph: %s (import %s, errors %v)", raw, status, failures)
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
