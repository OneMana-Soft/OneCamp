//go:build integration
// +build integration

package integration_test

// A whole Slack export comes across (chat item 2: a Slack team moves in an
// afternoon), against real Postgres, Redis and Dgraph. The export is
// testdata/slack_export/acme: written to Slack's documented export format
// (users, public and private channels, a DM, a group DM, and one day of each),
// not recorded from a live workspace, which only the owner can set up. It
// holds what a team's history holds: a join and a topic, formatting, a quote,
// code, a link, a thread with a reply sent to the channel too, reactions, an
// app's message, a /me message, an edit, a pin, an archived channel, a private
// channel, a DM, a group DM, and a deactivated person.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAWholeSlackExportComesAcross -v

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// zipTree is the files under dir as a zip, at their paths below it.
func zipTree(t *testing.T, dir string) []byte {
	t.Helper()
	return zipTreeWhere(t, dir, func(string) bool { return true })
}

// zipTreeWhere is zipTree of the files keep keeps, by their paths below dir.
func zipTreeWhere(t *testing.T, dir string, keep func(rel string) bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || !keep(filepath.ToSlash(rel)) {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		_, err = f.Write(body)
		return err
	})
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// acmeDir is the acme export, as Slack writes one.
var acmeDir = filepath.Join("..", "..", "testdata", "slack_export", "acme")

// acmeWorld is the workspace the acme export is imported into: an admin who
// imports it, and Ada and Bo already in it (Cy, deactivated in Slack, and
// GitHub's app come across as the import makes them).
type acmeWorld struct {
	e      *raceEnv
	dg     *integration.DgraphEnv
	admin  userModels.UserInfo
	people map[string]uuid.UUID
}

// newAcmeWorld sets the workspace up, with objects in the store.
func newAcmeWorld(t *testing.T, objects map[string][]byte) *acmeWorld {
	t.Helper()
	e := newRaceEnv(t, objects)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	if _, err := e.PG.Exec(`UPDATE users SET username = 'admin', is_admin = true WHERE id = $1`, e.admin); err != nil {
		t.Fatal(err)
	}
	// A workspace admin, as the routes ask (the admin flag is read from here).
	if _, err := e.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ('admin@acme.test')`); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": e.admin.String(), "user_name": "admin"})
	// Ada and Bo are in the workspace already; Cy (deactivated in Slack) and
	// GitHub's app come across as the import makes them.
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
	return &acmeWorld{e: e, dg: dg, admin: *signedIn(t, e.admin), people: people}
}

// asAdmin is a request's context, signed in as the world's admin.
func (w *acmeWorld) asAdmin() context.Context {
	return context.WithValue(context.Background(), helpers.UserInfoContextKey, w.admin)
}

func TestAWholeSlackExportComesAcross(t *testing.T) {
	ctx := context.Background()
	w := newAcmeWorld(t, map[string][]byte{"raw.zip": zipTree(t, acmeDir)})
	e, admin := w.e, w.admin
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
	start := time.Now()
	slackBusiness.RunImport(ctx, job, &admin)
	w.cameAcross(t, job, time.Since(start))
}

// cameAcross checks what the import job brought in of the acme export, and
// answers the codes it noted, by how many times.
func (w *acmeWorld) cameAcross(t *testing.T, job uuid.UUID, took time.Duration) map[string]int {
	t.Helper()
	e, dg, people := w.e, w.dg, w.people

	// The import's own word: finished, and what it noted.
	var status string
	if err := e.PG.QueryRow(`SELECT status FROM import_jobs WHERE id = $1`, job).Scan(&status); err != nil {
		t.Fatal(err)
	}
	notes := map[string]int{}
	rows, err := e.PG.Query(`SELECT COALESCE(code, ''), severity, message FROM import_errors WHERE import_id = $1`, job)
	if err != nil {
		t.Fatal(err)
	}
	var noted []string
	for rows.Next() {
		var code, severity, msg string
		_ = rows.Scan(&code, &severity, &msg)
		notes[code]++
		noted = append(noted, severity+" "+code+": "+msg)
	}
	rows.Close()
	sort.Strings(noted)
	t.Logf("the import: %s in %v, noted %v", status, took.Round(time.Millisecond), noted)
	// What each chunk did, and the import's counts, for a failure's reader.
	var progress string
	_ = e.PG.QueryRow(`SELECT COALESCE(progress::text, '') FROM import_jobs WHERE id = $1`, job).Scan(&progress)
	t.Logf("progress: %s", progress)
	chunks, err := e.PG.Query(`SELECT chunk_type, COALESCE(channel_slack_id, ''), COALESCE(object_key, ''), status, items_done, COALESCE(error, '')
		FROM import_chunks WHERE import_id = $1 ORDER BY created_at`, job)
	if err != nil {
		t.Fatal(err)
	}
	for chunks.Next() {
		var kind, slackID, key, st, chunkErr string
		var items int
		_ = chunks.Scan(&kind, &slackID, &key, &st, &items, &chunkErr)
		t.Logf("chunk %s %s %s: %s, %d items %s", kind, slackID, key, st, items, chunkErr)
	}
	chunks.Close()
	if status != "completed" {
		t.Errorf("the import ended %q", status)
	}
	for _, failed := range []string{"MESSAGE_IMPORT_FAILED", "COMMENT_IMPORT_FAILED", "CHAT_IMPORT_FAILED", "CHANNEL_IMPORT_FAILED", "PIN_IMPORT_FAILED"} {
		if notes[failed] > 0 {
			t.Errorf("the import noted %d %s", notes[failed], failed)
		}
	}

	// The graph: every channel with its posts, lines, replies and reactions,
	// and every DM and group with its messages.
	var graph struct {
		Channels []struct {
			UUID    string `json:"ch_uuid"`
			Name    string `json:"ch_name"`
			Private *bool  `json:"ch_private"`
			Members []struct {
				Name string `json:"user_name"`
			} `json:"ch_members"`
			Moderators []struct {
				Name string `json:"user_name"`
			} `json:"ch_moderators"`
			Posts []struct {
				Text string `json:"post_text"`
				Kind string `json:"post_kind"`
				By   struct {
					Name string `json:"user_name"`
				} `json:"post_by"`
				Reactions []struct {
					Emoji string `json:"reaction_emoji_id"`
					By    struct {
						Name string `json:"user_name"`
					} `json:"reaction_added_by"`
				} `json:"post_reactions"`
				Comments []struct {
					Text string `json:"comment_text"`
					By   struct {
						Name string `json:"user_name"`
					} `json:"comment_by"`
					Reactions []struct {
						Emoji string `json:"reaction_emoji_id"`
					} `json:"comment_reactions"`
				} `json:"post_comments"`
			} `json:"ch_posts"`
		} `json:"channels"`
		DMs []struct {
			Grouping string `json:"dm_grouping_id"`
			People   []struct {
				Name string `json:"user_name"`
			} `json:"dm_participants"`
			Chats []struct {
				Text string `json:"chat_body_text"`
				From struct {
					Name string `json:"user_name"`
				} `json:"chat_from"`
				Reactions []struct {
					Emoji string `json:"reaction_emoji_id"`
				} `json:"chat_reactions"`
			} `json:"dm_chats"`
		} `json:"dms"`
	}
	raw := dg.Query(t, `{
		channels(func: has(ch_uuid)) {
			ch_uuid ch_name ch_private
			ch_members { user_name }
			ch_moderators { user_name }
			ch_posts(orderasc: post_created_at) {
				post_text post_kind
				post_by { user_name }
				post_reactions { reaction_emoji_id reaction_added_by { user_name } }
				post_comments(orderasc: comment_created_at) { comment_text comment_by { user_name } comment_reactions { reaction_emoji_id } }
			}
		}
		dms(func: has(dm_grouping_id)) {
			dm_grouping_id
			dm_participants { user_name }
			dm_chats(orderasc: chat_created_at) { chat_body_text chat_from { user_name } chat_reactions { reaction_emoji_id } }
		}
	}`)
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	channels := map[string]int{}
	for i, c := range graph.Channels {
		channels[c.Name] = i
	}
	for _, name := range []string{"general", "launch", "old-launch", "leads"} {
		if _, ok := channels[name]; !ok {
			t.Errorf("#%s didn't come across (channels %v)", name, channels)
		}
	}
	if t.Failed() {
		t.Fatalf("the graph: %s", raw)
	}
	general := graph.Channels[channels["general"]]
	var messages, lines []string
	for _, p := range general.Posts {
		if p.Kind == "system" {
			lines = append(lines, helpersText(p.Text))
			continue
		}
		messages = append(messages, p.By.Name+": "+helpersText(p.Text))
	}
	t.Logf("#general: messages %q, lines %q", messages, lines)
	if len(lines) != 2 {
		t.Errorf("#general's lines %q, want the join and the topic", lines)
	}
	find := func(sub string) int {
		for i, p := range general.Posts {
			if strings.Contains(p.Text, sub) {
				return i
			}
		}
		t.Errorf("#general has no message with %q: %q", sub, messages)
		return -1
	}
	if i := find("House rules"); i >= 0 {
		p := general.Posts[i]
		if p.By.Name != "ada" || !strings.Contains(p.Text, "<br") || !strings.Contains(p.Text, "<blockquote>") || len(p.Reactions) != 1 || p.Reactions[0].By.Name != "bo" {
			t.Errorf("Ada's welcome: by %q, %q, reactions %+v", p.By.Name, p.Text, p.Reactions)
		}
	}
	if i := find("deploy_v2</code>"); i >= 0 {
		p := general.Posts[i]
		var replies []string
		for _, c := range p.Comments {
			replies = append(replies, c.By.Name+": "+helpersText(c.Text))
		}
		t.Logf("the thread: %q", replies)
		if len(p.Comments) != 3 {
			t.Errorf("the thread's replies %q, want Ada's, Bo's and the one sent to the channel too", replies)
		} else if len(p.Comments[1].Reactions) != 1 {
			t.Errorf("Bo's reply's reactions %+v, want Ada's +1", p.Comments[1].Reactions)
		}
	}
	if i := find("PR #12 merged"); i >= 0 && !strings.HasPrefix(general.Posts[i].Text, `<p><span class="bot-attrib">[bot: GitHub]</span>`) {
		t.Errorf("GitHub's message: %q", general.Posts[i].Text)
	}
	if i := find("is out this afternoon"); i >= 0 && !strings.Contains(general.Posts[i].Text, "<em>is out this afternoon</em>") {
		t.Errorf("Bo's /me message isn't in italics: %q", general.Posts[i].Text)
	}
	find("Standup moved to 10:30")
	// The reply Ada sent to the channel too is in the channel as well.
	find("we launch on Friday")

	launch := graph.Channels[channels["launch"]]
	if len(launch.Posts) != 2 || !strings.Contains(launch.Posts[1].Text, "<del>Thursday</del>") || !strings.Contains(launch.Posts[0].Text, `data-type="channelMention"`) {
		t.Errorf("#launch: %+v", launch.Posts)
	}
	var pinnedBy uuid.UUID
	var pinnedAt time.Time
	if err := e.PG.QueryRow(`SELECT pinned_by, pinned_at FROM message_pins WHERE conversation_kind = 'channel'`).Scan(&pinnedBy, &pinnedAt); err != nil || pinnedBy != people["bo"] || !pinnedAt.Equal(time.Unix(1759395660, 0)) {
		t.Errorf("#launch's pin: by %s at %v (%v), want Bo at %v", pinnedBy, pinnedAt, err, time.Unix(1759395660, 0).UTC())
	}
	if leads := graph.Channels[channels["leads"]]; leads.Private == nil || !*leads.Private || len(leads.Posts) != 1 || !strings.Contains(leads.Posts[0].Text, "<pre><code>engineers: 2\ndesigners: 1</code></pre>") {
		t.Errorf("#leads: private %v, posts %+v", leads.Private, leads.Posts)
	}
	if old := graph.Channels[channels["old-launch"]]; len(old.Posts) != 1 {
		t.Errorf("#old-launch: %+v", old.Posts)
	}

	// The DM and the group DM, with their messages.
	var dm, group int = -1, -1
	for i, d := range graph.DMs {
		switch len(d.People) {
		case 2:
			dm = i
		case 3:
			group = i
		}
	}
	if dm < 0 || len(graph.DMs[dm].Chats) != 2 || len(graph.DMs[dm].Chats[1].Reactions) != 1 {
		t.Errorf("the DM: %s", raw)
	}
	if group < 0 || len(graph.DMs[group].Chats) != 2 {
		t.Errorf("the group DM: %s", raw)
	} else if from := graph.DMs[group].Chats[1].From.Name; from == "" || from == "admin" {
		t.Errorf("Cy's message in the group came across as %q", from)
	}

	// Who is in what. Each conversation holds exactly its Slack members who
	// came across (Cy, deactivated in Slack, as slack-cy, the import made),
	// and the admin who ran the import is in none they weren't in: no member,
	// no moderator. Nobody opens a DM, a group DM or a private channel they
	// weren't in: the admin can't, Ada can.
	names := func(people []struct {
		Name string `json:"user_name"`
	}) []string {
		var out []string
		for _, p := range people {
			out = append(out, p.Name)
		}
		sort.Strings(out)
		return out
	}
	for name, want := range map[string][]string{"general": {"ada", "bo", "slack-cy"}, "launch": {"ada", "bo"}, "old-launch": {"ada"}, "leads": {"ada", "bo"}} {
		c := graph.Channels[channels[name]]
		if got := names(c.Members); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("#%s's members %v, want %v", name, got, want)
		}
		if mods := names(c.Moderators); slices.Contains(mods, "admin") {
			t.Errorf("the admin who ran the import moderates #%s: %v", name, mods)
		}
	}
	var refs []conversationBusiness.Ref
	if dm >= 0 {
		if got := names(graph.DMs[dm].People); fmt.Sprint(got) != "[ada bo]" {
			t.Errorf("the DM's people %v, want Ada and Bo", got)
		}
		refs = append(refs, conversationBusiness.ChatRef(graph.DMs[dm].Grouping))
	}
	if group >= 0 {
		if got := names(graph.DMs[group].People); fmt.Sprint(got) != "[ada bo slack-cy]" {
			t.Errorf("the group DM's people %v, want Ada, Bo and Cy", got)
		}
		refs = append(refs, conversationBusiness.ChatRef(graph.DMs[group].Grouping))
	}
	refs = append(refs, conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: graph.Channels[channels["leads"]].UUID})
	ctx := context.Background()
	ada := signedIn(t, people["ada"])
	for _, ref := range refs {
		if _, err := conversationBusiness.Resolve(ctx, &w.admin, ref); !errors.Is(err, conversationBusiness.ErrNotFound) {
			t.Errorf("the admin who ran the import opens %s %s: %v", ref.Kind, ref.ID, err)
		}
		if _, err := conversationBusiness.Resolve(ctx, ada, ref); err != nil {
			t.Errorf("Ada can't open %s %s: %v", ref.Kind, ref.ID, err)
		}
	}
	return notes
}

// signedIn is the person id as the server knows them signed in.
func signedIn(t *testing.T, id uuid.UUID) *userModels.UserInfo {
	t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", id, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
	if err != nil || gr == nil {
		t.Fatalf("load %s's node: %v", id, err)
	}
	return &userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
}

// helpersText is html's words, for a log line.
func helpersText(html string) string {
	var b strings.Builder
	in := false
	for _, r := range html {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
