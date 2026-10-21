//go:build integration
// +build integration

package integration_test

// Imported Slack messages read as they did in Slack (chat item 2), against
// real Postgres, Redis and Dgraph: a message's lines and quote stay lines and
// a quote, its code and its link's address are left as written, a mention of
// someone who came across is OneCamp's own chip, and an app's message is led
// by the app's name in words. An edit of either keeps its people: the edit
// path's mention rule (business/Send Mentions) reads each chip as the person
// it names, where it refused both, as "unregistered users", before.
//
// Run: go test -tags=integration ./tests/integration/ -run TestImportedSlackMessagesReadAsTheyDidInSlack -v

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestImportedSlackMessagesReadAsTheyDidInSlack(t *testing.T) {
	ctx := context.Background()

	// #launch: Ada thanks Bo, with a code word, a link and a quote; GitHub's
	// app says a pull request merged.
	var export bytes.Buffer
	zw := zip.NewWriter(&export)
	for name, body := range map[string]string{
		"users.json": `[{"id":"U1","name":"ada","real_name":"Ada","profile":{"email":"ada@acme.test","real_name":"Ada"}},
			{"id":"U2","name":"bo","real_name":"Bo","profile":{"email":"bo@acme.test","real_name":"Bo"}}]`,
		"channels.json": `[{"id":"C1","name":"launch","created":1759300000,"creator":"U1","is_archived":false,"is_general":false,
			"members":["U1","U2"],"topic":{"value":""},"purpose":{"value":""}}]`,
		"launch/2025-10-01.json": `[
			{"type":"message","user":"U1","text":"Thanks <@U2>!\nSee ` + "`my_var_name`" + ` at <https://example.com/a_b_c|the docs>\n\n&gt; ship it","ts":"1759309200.000100"},
			{"type":"message","subtype":"bot_message","bot_id":"B1","username":"GitHub","bot_profile":{"name":"GitHub"},"text":"PR merged by <@U1>","ts":"1759309260.000200"}]`,
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
	people := map[string]uuid.UUID{}
	for _, name := range []string{"ada", "bo"} {
		id := uuid.New()
		people[name] = id
		if _, err := e.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_email_id": name + "@acme.test"})
	}
	load := func(id uuid.UUID) userModels.UserInfo {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", id, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", id, err)
		}
		return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
	}
	admin := load(e.admin)

	job := e.job(t, "slack", "Acme", "planned", "slackImport/acme/raw.zip")
	if _, err := e.PG.Exec(`UPDATE import_jobs SET options = '{"skip_subtypes": false}' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
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

	// The two posts, and Ada's and Bo's graph uids.
	var graph struct {
		Posts []struct {
			Text string `json:"post_text"`
		} `json:"posts"`
		People []struct {
			UID  string `json:"uid"`
			UUID string `json:"user_uuid"`
		} `json:"people"`
	}
	raw := dg.Query(t, `{
		posts(func: has(post_uuid)) { post_text }
		people(func: has(user_uuid)) { uid user_uuid }
	}`)
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	uid := map[string]string{}
	for _, p := range graph.People {
		uid[p.UUID] = p.UID
	}
	var thanks, merged string
	for _, p := range graph.Posts {
		switch {
		case strings.Contains(p.Text, "Thanks"):
			thanks = p.Text
		case strings.Contains(p.Text, "PR merged"):
			merged = p.Text
		}
	}
	if thanks == "" || merged == "" {
		t.Fatalf("the messages didn't come across (import %s): %s", status, raw)
	}
	bo, ada := people["bo"].String(), people["ada"].String()
	for _, want := range []string{
		`data-type="mention" data-id="` + bo + `@` + uid[bo] + `" data-label="bo">@bo</span>!<br`,
		`<code>my_var_name</code>`,
		`href="https://example.com/a_b_c"`,
		`<blockquote><p>ship it</p></blockquote>`,
	} {
		if !strings.Contains(thanks, want) {
			t.Errorf("Ada's message %q\nlacks %q", thanks, want)
		}
	}
	if strings.Contains(thanks, "<em>") {
		t.Errorf("emphasis in Ada's code or link: %q", thanks)
	}
	if !strings.HasPrefix(merged, `<p><span class="bot-attrib">[bot: GitHub]</span> PR merged by <span class="mention" data-type="mention" data-id="`+ada+`@`+uid[ada]+`"`) {
		t.Errorf("GitHub's message: %q", merged)
	}

	// An edit of either keeps its people, as Ada edits them.
	channel, err := uuid.Parse(mapped(t, e, job, "channel", "C1"))
	if err != nil {
		t.Fatal(err)
	}
	editor := load(people["ada"])
	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channel, editor.UserDgraphInfo.Uid)
	if err != nil || ch == nil {
		t.Fatalf("read the channel: %v", err)
	}
	for _, html := range []string{thanks, merged} {
		edited := html + "<p>Edited.</p>"
		named, err := sendBusiness.Mentions(ctx, &editor, sendBusiness.Where{Channel: ch, Before: html}, &edited)
		if err != nil {
			t.Errorf("an edit of %q was refused: %v", html, err)
			continue
		}
		if len(named) != 1 {
			t.Errorf("an edit of %q names %d people, want the one its chip names", html, len(named))
		}
	}
}

// mapped is what an import made of a Slack id, by its id map.
func mapped(t *testing.T, e *raceEnv, job uuid.UUID, entity, slackID string) string {
	t.Helper()
	var id string
	if err := e.PG.QueryRow(`SELECT onecamp_uuid::text FROM import_id_map WHERE import_id = $1 AND entity_type = $2 AND source_id = $3`,
		job, entity, slackID).Scan(&id); err != nil {
		t.Fatalf("the import's %s %s: %v", entity, slackID, err)
	}
	return id
}
