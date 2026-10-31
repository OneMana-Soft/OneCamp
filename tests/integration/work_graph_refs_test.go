//go:build integration
// +build integration

package integration_test

// The work graph's references (DG6) through the real write path, the outbox
// and the Connected panel's handler, against Postgres 12 and Dgraph with the
// production schema.
//
//   - olu and mia are in the public #launch; only olu is in the private #exec;
//     out is in the workspace and nothing else;
//   - Plan is a workspace doc; Task is in project P, which mia is in.
//
// A message is edited (business/Post UpdatePost, the edit any member makes)
// to link Plan: once the outbox has run, Plan's Connected panel names it, and
// the message names Plan and Task. An edit that takes the link out takes the
// edge out. A message in #exec linking Plan is shown to olu and never to mia
// or out, nor is #exec. This file uses only what was there before DG6, so it
// runs (and fails) on the code before it.
//
// Run: integ_rel.sh <wt> <log> '^TestWorkGraphReferences$'

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	workGraphController "github.com/akashc777/OneCamp/controllers/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// refsWorld is the people, things and handler calls the reference tests share.
type refsWorld struct {
	t      *testing.T
	env    *integration.Env
	dg     *integration.DgraphEnv
	people map[string]uuid.UUID
	id     map[string]string
	uids   map[string]string
	name   map[string]string
}

func newRefsWorld(t *testing.T) *refsWorld {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	// Writes reach a search that takes them (a meeting's calendar entry, say),
	// so the outbox events settleOutbox waits on are delivered.
	integration.AcceptingOpenSearch(t)
	w := &refsWorld{t: t, env: env, dg: integration.SetupDgraph(t), people: map[string]uuid.UUID{}, id: map[string]string{}, name: map[string]string{}}
	zero := "0001-01-01T00:00:00Z"
	var nodes []map[string]any
	for _, n := range []string{"olu", "mia", "out"} {
		w.people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			w.people[n], w.people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": w.people[n].String(), "user_name": n, "user_deleted_at": zero})
	}
	for _, k := range []string{"plan", "proj", "task", "launch", "exec", "m1", "m2", "m3"} {
		w.id[k] = uuid.NewString()
	}
	ref := func(key string) map[string]any { return map[string]any{"uid": "_:" + key} }
	post := func(key, ch, text string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Post", "post_uuid": w.id[key], "post_text": text, "post_deleted_at": zero,
			"post_created_at": "2026-10-10T10:00:00Z", "post_by": ref("olu"), "post_channel": ref(ch)}
	}
	nodes = append(nodes,
		map[string]any{"uid": "_:plan", "dgraph.type": "Doc", "doc_uuid": w.id["plan"], "doc_title": "Plan", "doc_private": false,
			"doc_deleted_at": zero, "doc_owner": ref("olu"), "doc_created_by": ref("olu")},
		map[string]any{"uid": "_:proj", "dgraph.type": "Project", "project_uuid": w.id["proj"], "project_name": "P", "project_deleted_at": zero,
			"project_members": []map[string]any{ref("olu"), ref("mia")}, "project_admins": []map[string]any{ref("olu")},
			"project_tasks": []map[string]any{ref("task")}},
		map[string]any{"uid": "_:task", "dgraph.type": "Task", "task_uuid": w.id["task"], "task_name": "Record the film", "task_status": "todo",
			"task_deleted_at": zero, "task_project": ref("proj"), "task_created_by": ref("olu"), "task_assignee": ref("mia")},
		map[string]any{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": w.id["launch"], "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": []map[string]any{ref("olu"), ref("mia")}, "ch_posts": []map[string]any{ref("m1"), ref("m3")}},
		map[string]any{"uid": "_:exec", "dgraph.type": "Channel", "ch_uuid": w.id["exec"], "ch_name": "exec", "ch_private": true,
			"ch_deleted_at": zero, "ch_members": []map[string]any{ref("olu")}, "ch_posts": []map[string]any{ref("m2")}},
		post("m1", "launch", "<p>Draft coming</p>"),
		post("m2", "exec", "<p>Budget soon</p>"),
		post("m3", "launch", "<p>Lunch?</p>"),
	)
	w.uids = w.dg.Mutate(t, nodes)
	for k, v := range w.id {
		w.name[v] = k
	}
	for k, v := range w.people {
		w.name[v.String()] = k
	}
	return w
}

// as is a request context for a person; "tok" is mia through an API token
// (a token reads as the member it belongs to).
func (w *refsWorld) as(person string) context.Context {
	if person == "tok" {
		return helpers.WithActsThroughToken(w.as("mia"))
	}
	var u userModels.UserInfo
	u.UserPostgresInfo.Id = w.people[person]
	u.UserPostgresInfo.EmailID = w.people[person].String() + "@example.test"
	u.UserPostgresInfo.IsAdmin = person == "olu"
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[person], Uuid: w.people[person].String(), UserName: person}
	return context.WithValue(context.Background(), helpers.UserInfoContextKey, u)
}

// connected is what person's Connected panel lists for a thing, one step
// out, as "kind name via rel>name", sorted, and the raw answer.
func (w *refsWorld) connected(person, kind, key string) ([]string, string) {
	w.t.Helper()
	return w.connectedKinds(person, kind, key, "")
}

// connectedKinds is connected for the kinds named ("doc,event"; "" for the
// panel's own).
func (w *refsWorld) connectedKinds(person, kind, key, kinds string) ([]string, string) {
	w.t.Helper()
	q := url.Values{"kind": {kind}, "uuid": {w.id[key]}, "depth": {"1"}}
	if kinds != "" {
		q.Set("kinds", kinds)
	}
	req := httptest.NewRequest("GET", "/graph/connected?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	workGraphController.GetConnected(rec, req.WithContext(w.as(person)))
	if rec.Code != 200 {
		w.t.Fatalf("%s's panel on %s: %d %s", person, key, rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	var out []string
	data, _ := body["data"].(map[string]any)
	groups, _ := data["groups"].([]any)
	for _, g := range groups {
		for _, it := range g.(map[string]any)["items"].([]any) {
			im := it.(map[string]any)
			var hops []string
			for _, h := range im["path"].([]any) {
				hm := h.(map[string]any)
				hops = append(hops, hm["rel"].(string)+">"+w.name[hm["id"].(string)])
			}
			out = append(out, im["kind"].(string)+" "+w.name[im["id"].(string)]+" via "+strings.Join(hops, " "))
		}
	}
	sort.Strings(out)
	return out, rec.Body.String()
}

// path is how many ways person's GET /graph/path finds from one thing to
// another, or minus the status when it isn't 200.
func (w *refsWorld) path(person, fromKind, from, toKind, to string) int {
	w.t.Helper()
	q := url.Values{"from_kind": {fromKind}, "from": {w.id[from]}, "to_kind": {toKind}, "to": {w.id[to]}}
	rec := httptest.NewRecorder()
	workGraphController.GetPath(rec, httptest.NewRequest("GET", "/graph/path?"+q.Encode(), nil).WithContext(w.as(person)))
	if rec.Code != 200 {
		return -rec.Code
	}
	var body struct {
		Data struct {
			Paths []any `json:"paths"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return len(body.Data.Paths)
}

// edit edits a message as its author does (business/Post UpdatePost) and
// waits for the outbox to have run every event the edit wrote. The edit runs
// as an import's would, so nobody is sent it live (no broker here).
func (w *refsWorld) edit(key, channel, html string) {
	w.t.Helper()
	ctx := context.WithValue(w.as("olu"), helpers.BulkImportContextKey, true)
	in := &adapter.InputCreateOrUpdatePostInfo{HTMLText: html, ChannelUUID: uuid.MustParse(w.id[channel])}
	if err := postBusiness.UpdatePost(ctx, in, nil, uuid.MustParse(w.id[key]), w.id[channel], w.people["olu"].String()); err != nil {
		w.t.Fatalf("edit %s: %v", key, err)
	}
	w.settleOutbox()
}

// settleOutbox runs the drain until no event waits (a writer's own delivery
// may be running beside it).
func (w *refsWorld) settleOutbox() {
	w.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_ = outboxBusiness.Drain(context.Background())
		var waiting int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM outbox WHERE state IN ('held', 'ready') OR claimed_until > NOW()`).Scan(&waiting); err != nil {
			w.t.Fatal(err)
		}
		if waiting == 0 {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("%d outbox events still wait after 30 s:%s", waiting, w.waitingOutbox())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitingOutbox lists the events settleOutbox still waits on, so a timeout
// names who wrote them (topic, kind, key) and why they wait: held by a writer
// that never released them, ready but not due yet after a failed delivery, or
// claimed by a delivery still running.
func (w *refsWorld) waitingOutbox() string {
	rows, err := w.env.PG.Query(`SELECT id, topic, kind, key, state, attempts,
		EXTRACT(EPOCH FROM NOW() - created_at)::int,
		EXTRACT(EPOCH FROM available_at - NOW())::int,
		COALESCE(EXTRACT(EPOCH FROM claimed_until - NOW())::int, 0),
		array_to_string(sinks, ','), array_to_string(sinks_done, ','), COALESCE(last_error, '')
		FROM outbox WHERE state IN ('held', 'ready') OR claimed_until > NOW() ORDER BY id LIMIT 20`)
	if err != nil {
		return " (they couldn't be listed: " + err.Error() + ")"
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id int64
		var attempts, age, dueIn, claimedFor int
		var topic, kind, key, state, sinks, done, lastErr string
		if err := rows.Scan(&id, &topic, &kind, &key, &state, &attempts, &age, &dueIn, &claimedFor, &sinks, &done, &lastErr); err != nil {
			return b.String() + " (a row couldn't be read: " + err.Error() + ")"
		}
		fmt.Fprintf(&b, "\n  #%d %s/%s key %q: %s, %d attempts, written %d s ago, due in %d s, claimed for %d s, sinks [%s] done [%s]", id, topic, kind, key, state, attempts, age, dueIn, claimedFor, sinks, done)
		if lastErr != "" {
			if len(lastErr) > 200 {
				lastErr = lastErr[:200] + "…"
			}
			fmt.Fprintf(&b, ", last error: %s", lastErr)
		}
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(&b, " (the list stopped early: %v)", err)
	}
	return b.String()
}

func refsHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestWorkGraphReferences(t *testing.T) {
	w := newRefsWorld(t)
	chip := func(kind, key string) string {
		return fmt.Sprintf(`<span data-type="entityMention" data-id="%s:%s" data-label="x"></span>`, kind, w.id[key])
	}

	t.Run("a message linking a doc shows in the doc's connections", func(t *testing.T) {
		w.edit("m1", "launch", "<p>The plan: "+chip("doc", "plan")+` and <a href="https://camp.example/app/task/`+w.id["task"]+`">the film</a></p>`)
		got, raw := w.connected("mia", "doc", "plan")
		if !refsHas(got, "message m1 via linked_from>m1") {
			t.Errorf("mia's panel on Plan doesn't name the message that links it:\n%s\n%s", strings.Join(got, "\n"), raw)
		}
		got, _ = w.connected("mia", "message", "m1")
		for _, want := range []string{"doc plan via links_to>plan", "task task via links_to>task"} {
			if !refsHas(got, want) {
				t.Errorf("the message's panel lacks %q:\n%s", want, strings.Join(got, "\n"))
			}
		}
		got, _ = w.connected("mia", "task", "task")
		if !refsHas(got, "message m1 via linked_from>m1") {
			t.Errorf("the task's panel doesn't name the message that links it:\n%s", strings.Join(got, "\n"))
		}
	})

	t.Run("an edit that takes the link out takes the edge out", func(t *testing.T) {
		w.edit("m1", "launch", `<p>The film: <a href="/app/task/`+w.id["task"]+`">here</a></p>`)
		got, _ := w.connected("olu", "doc", "plan")
		if refsHas(got, "message m1 via linked_from>m1") {
			t.Errorf("Plan still names the message after the link was taken out:\n%s", strings.Join(got, "\n"))
		}
		got, _ = w.connected("olu", "message", "m1")
		if refsHas(got, "doc plan via links_to>plan") || !refsHas(got, "task task via links_to>task") {
			t.Errorf("the message's links after the edit:\n%s", strings.Join(got, "\n"))
		}
		// The same edit again changes nothing.
		w.edit("m1", "launch", `<p>The film: <a href="/app/task/`+w.id["task"]+`">here</a></p>`)
		if again, _ := w.connected("olu", "message", "m1"); strings.Join(again, "\n") != strings.Join(got, "\n") {
			t.Errorf("a repeated edit changed the edges:\n%s\nthen\n%s", strings.Join(got, "\n"), strings.Join(again, "\n"))
		}
	})

	t.Run("a private channel's message linking a public doc is never shown outside the channel", func(t *testing.T) {
		w.edit("m2", "exec", "<p>Budget for "+chip("doc", "plan")+"</p>")
		got, _ := w.connected("olu", "doc", "plan")
		if !refsHas(got, "message m2 via linked_from>m2") {
			t.Errorf("olu, in #exec, doesn't see its message on Plan:\n%s", strings.Join(got, "\n"))
		}
		if p := w.path("mia", "doc", "plan", "message", "m2"); p != -404 {
			t.Errorf("mia's path from Plan to #exec's message: %d, want not found", p)
		}
		if p := w.path("olu", "doc", "plan", "message", "m2"); p != 1 {
			t.Errorf("olu's ways from Plan to #exec's message: %d, want 1", p)
		}
		for _, who := range []string{"mia", "out", "tok"} {
			got, raw := w.connected(who, "doc", "plan")
			for _, k := range []string{"m2", "exec"} {
				if strings.Contains(raw, w.id[k]) {
					t.Errorf("%s's panel on Plan names %s:\n%s", who, k, strings.Join(got, "\n"))
				}
			}
		}
	})

	t.Run("an edit with no link makes no edge, with one event", func(t *testing.T) {
		var before, after int
		_ = w.env.PG.QueryRow(`SELECT count(*) FROM outbox`).Scan(&before)
		// An edit always writes one (it may take a link out); this is about the
		// graph: the panel of a message with no link names nothing it links.
		w.edit("m3", "launch", "<p>Lunch at one?</p>")
		_ = w.env.PG.QueryRow(`SELECT count(*) FROM outbox`).Scan(&after)
		got, _ := w.connected("mia", "message", "m3")
		for _, g := range got {
			if strings.Contains(g, "links_to") {
				t.Errorf("a message with no link links %s", g)
			}
		}
		if after-before > 1 {
			t.Errorf("one edit wrote %d events", after-before)
		}
	})
}
