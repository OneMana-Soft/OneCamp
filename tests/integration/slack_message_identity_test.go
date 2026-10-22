//go:build integration
// +build integration

package integration_test

// A Slack message is known by its conversation and its ts (chat item 2). Slack's
// ts is unique within a conversation, not across them: two conversations can
// each hold a message at the same ts. The import knew a message by its ts
// alone, in its own map and the workspace's, so the second of two such
// messages was taken as already imported and dropped, and a reply to one could
// be filed under the other: a private channel's reply in a public channel's
// thread, for everyone there to read.
//
// The export here holds three messages at one ts: Ada's plan in #general
// (public), her interview notes in #hiring (private, hers alone) with a reply
// she wrote under them, and Bo's lunch question in their DM. Each must come
// across in its own conversation, the reply under the notes, and nothing of
// #hiring or the DM anywhere else.
//
// Run: go test -tags=integration ./tests/integration/ -run TestMessagesAtOneTsInTwoConversationsStayApart -v

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"

	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	"github.com/akashc777/OneCamp/tests/integration"
)

// writeExport writes files (path below dir → JSON value) as an export's.
func writeExport(t *testing.T, files map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range files {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// collidingExport is an export with three messages at one ts (collidingTs):
// Ada's plan in #general (public, C0GENERAL1), her interview notes in #hiring
// (private, hers alone, G0HIRING1) with her reply under them (replyTs), and
// Bo's lunch question in their DM (D0ADABO01).
const (
	collidingTs = "1759310000.000100"
	replyTs     = "1759310100.000200"
)

func collidingExport(t *testing.T) string {
	t.Helper()
	const ada, bo, at, later = "U0ADA0001", "U0BO00002", collidingTs, replyTs
	person := func(id, name string) map[string]any {
		return map[string]any{"id": id, "team_id": "T0ACME001", "name": name, "real_name": name,
			"profile": map[string]any{"real_name": name, "display_name": name, "email": name + "@acme.test"}}
	}
	return writeExport(t, map[string]any{
		"users.json": []any{person(ada, "ada"), person(bo, "bo")},
		"channels.json": []any{map[string]any{"id": "C0GENERAL1", "name": "general", "created": 1759000000, "creator": ada,
			"is_general": true, "members": []string{ada, bo}}},
		"groups.json": []any{map[string]any{"id": "G0HIRING1", "name": "hiring", "created": 1759000100, "creator": ada,
			"members": []string{ada}}},
		"dms.json":   []any{map[string]any{"id": "D0ADABO01", "created": 1759000200, "members": []string{ada, bo}}},
		"mpims.json": []any{},
		"general/2025-10-01.json": []any{
			map[string]any{"type": "message", "ts": at, "user": ada, "text": "Plan for Friday"},
		},
		"hiring/2025-10-01.json": []any{
			map[string]any{"type": "message", "ts": at, "user": ada, "text": "Interview notes", "thread_ts": at, "reply_count": 1},
			map[string]any{"type": "message", "ts": later, "user": ada, "text": "Candidate two is strong", "thread_ts": at},
		},
		"D0ADABO01/2025-10-01.json": []any{
			map[string]any{"type": "message", "ts": at, "user": bo, "text": "Lunch?"},
		},
	})
}

func TestMessagesAtOneTsInTwoConversationsStayApart(t *testing.T) {
	dir := collidingExport(t)
	ctx := context.Background()
	w := newAcmeWorld(t, map[string][]byte{"raw.zip": zipTree(t, dir)})
	job := w.e.job(t, "slack", "Acme", "planned", "slackImport/acme/raw.zip")
	arc, done, err := slackBusiness.OpenStagedZipFromMinIO(ctx, "slackImport/acme/raw.zip")
	if err != nil {
		t.Fatalf("open the export: %v", err)
	}
	if _, err := slackBusiness.BuildPlan(ctx, job, arc); err != nil {
		t.Fatalf("plan: %v", err)
	}
	done()
	slackBusiness.RunImport(ctx, job, &w.admin)
	if status, _ := w.e.state(t, job); status != "completed" {
		t.Fatalf("the import ended %q (noted %v)", status, codes(t, w.e, job))
	}

	var graph struct {
		Channels []struct {
			Name  string `json:"ch_name"`
			Posts []struct {
				Text     string `json:"post_text"`
				Comments []struct {
					Text string `json:"comment_text"`
				} `json:"post_comments"`
			} `json:"ch_posts"`
		} `json:"channels"`
		DMs []struct {
			Chats []struct {
				Text string `json:"chat_body_text"`
			} `json:"dm_chats"`
		} `json:"dms"`
	}
	raw := w.dg.Query(t, `{
		channels(func: has(ch_uuid)) { ch_name ch_posts { post_text post_comments { comment_text } } }
		dms(func: has(dm_grouping_id)) { dm_chats { chat_body_text } }
	}`)
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	got := map[string][]string{}
	for _, c := range graph.Channels {
		for _, p := range c.Posts {
			line := helpersText(p.Text)
			for _, r := range p.Comments {
				line += " / reply: " + helpersText(r.Text)
			}
			got["#"+c.Name] = append(got["#"+c.Name], line)
		}
	}
	for _, d := range graph.DMs {
		for _, m := range d.Chats {
			got["the DM"] = append(got["the DM"], helpersText(m.Text))
		}
	}
	for k := range got {
		sort.Strings(got[k])
	}
	want := map[string][]string{
		"#general": {"Plan for Friday"},
		"#hiring":  {"Interview notes / reply: Candidate two is strong"},
		"the DM":   {"Lunch?"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("came across %v\nwant %v\n(noted %v)", got, want, codes(t, w.e, job))
	}
}

// The rows imports wrote before a message was known by its conversation keep
// working: the migration keys each by its conversation where the row tells it
// (a channel message's channel, a DM message's grouping, a reply's thread),
// in the import's map and the workspace's, and a re-import finds them. A row
// it can't tell keeps its bare ts. Down puts the bare ts back.
func TestEarlierSlackImportsMessagesAreKeyedByConversation(t *testing.T) {
	env := integration.SetupEnv(t)
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	matches, err := filepath.Glob(filepath.Join(dir, "*_slack_message_keys.up.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the migration: %v %v", matches, err)
	}
	n, err := strconv.Atoi(strings.SplitN(filepath.Base(matches[0]), "_", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.New("file://"+dir, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for v, _, _ := m.Version(); v >= uint(n); v, _, _ = m.Version() {
		if err := m.Steps(-1); err != nil {
			t.Fatalf("down from %d: %v", v, err)
		}
	}

	// An import as it was written before: every message by its bare ts.
	job := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status) VALUES ($1, 'slack', 'Acme', 'export_zip', 'completed')`, job); err != nil {
		t.Fatal(err)
	}
	post, chat, reply, empty := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, r := range []struct {
		entity, id string
		to         uuid.UUID
		parent     any
		meta       string
	}{
		{"channel", "C1", uuid.New(), nil, `{"final_name": "general"}`},
		{"dm", "D1", uuid.New(), nil, `{"grouping_id": "g-1"}`},
		{"message", "100.1", post, nil, `{"channel_slack_id": "C1", "author_slack": "U1"}`},
		{"message", "100.2", chat, nil, `{"grouping_id": "g-1", "author_slack": "U2"}`},
		{"comment", "100.3", reply, "100.1", `{"thread_ts": "100.1"}`},
		{"message", "100.9", empty, nil, `{"empty": true}`},
	} {
		if _, err := env.PG.Exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid, parent_source_id, metadata)
			VALUES ($1, $2, $3, $4, $5, $6)`, job, r.entity, r.id, r.to, r.parent, r.meta); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct {
		entity, id string
		to         uuid.UUID
	}{{"message", "100.1", post}, {"message", "100.2", chat}, {"comment", "100.3", reply}} {
		if _, err := env.PG.Exec(`INSERT INTO import_workspace_id_map (provider, source_workspace_name, entity_type, source_id, onecamp_uuid, origin_import_id)
			VALUES ('slack', 'Acme', $1, $2, $3, $4)`, r.entity, r.id, r.to, job); err != nil {
			t.Fatal(err)
		}
	}
	keys := func() string {
		t.Helper()
		var out []string
		rows, err := env.PG.Query(`SELECT 'import ' || entity_type || ' ' || source_id || COALESCE(' under ' || parent_source_id, '') FROM import_id_map WHERE import_id = $1 AND entity_type IN ('message', 'comment')
			UNION ALL SELECT 'workspace ' || entity_type || ' ' || source_id FROM import_workspace_id_map WHERE source_workspace_name = 'Acme'`, job)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			_ = rows.Scan(&k)
			out = append(out, k)
		}
		sort.Strings(out)
		return fmt.Sprint(out)
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got, want := keys(), "[import comment C1/100.3 under C1/100.1 import message 100.9 import message C1/100.1 import message D1/100.2 workspace comment C1/100.3 workspace message C1/100.1 workspace message D1/100.2]"; got != want {
		t.Errorf("after the migration:\n got %s\nwant %s", got, want)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got, want := keys(), "[import comment 100.3 under 100.1 import message 100.1 import message 100.2 import message 100.9 workspace comment 100.3 workspace message 100.1 workspace message 100.2]"; got != want {
		t.Errorf("after the way back:\n got %s\nwant %s", got, want)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
