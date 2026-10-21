//go:build integration
// +build integration

package integration_test

// Who knows (DG9) through its handlers, against real Postgres 12, Dgraph with
// the production schema and OpenSearch mapped as the server maps it: who
// knows about "budget", as the asker may see it.
//
//   - ana asks. She is in project P and the public #launch, and nothing else;
//   - cy wrote the public Budget plan, created P's budget task, and wrote a
//     budget doc that is deleted;
//   - dee is assigned P's task;
//   - ben wrote the private Budget secret (ana can't open it), and commented
//     on the plan;
//   - gil only commented on the secret;
//   - fay only posted about the budget in #exec, a private channel ana isn't
//     in (her stale profile claims it, as a cached profile can);
//   - eve wrote a public budget doc, and has left herself out;
//   - a bot and an external account wrote and commented on budget docs;
//   - old wrote a budget doc and has been deleted.
//
// Every entry is in the index as a stale copy would have it (the secret
// public there), so only the graph's decision keeps anything out.
//
// Run: integ_rel.sh <wt> <log> '^TestWhoKnows$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	whoKnowsController "github.com/akashc777/OneCamp/controllers/WhoKnows"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWhoKnows(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	zero := "0001-01-01T00:00:00Z"
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range []string{"ana", "ben", "cy", "dee", "eve", "fay", "gil", "bot", "ext", "old"} {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		node := map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n,
			"user_email_id": people[n].String() + "@example.test", "user_deleted_at": zero}
		switch n {
		case "bot":
			node["is_bot"], node["is_external"] = true, true
		case "ext":
			node["is_external"] = true
		case "old":
			node["user_deleted_at"] = "2026-10-01T00:00:00Z"
		}
		nodes = append(nodes, node)
	}
	// Eve left herself out before anyone asked.
	if _, err := env.PG.Exec(`INSERT INTO who_knows_opt_outs (user_uuid) VALUES ($1)`, people["eve"]); err != nil {
		t.Fatal(err)
	}
	id := map[string]string{}
	for _, k := range []string{"plan", "secret", "gonedoc", "evedoc", "botdoc", "extdoc", "olddoc", "proj", "task", "launch", "exec", "m1", "c1", "c2", "c3", "c4"} {
		id[k] = uuid.NewString()
	}
	ref := func(key string) map[string]any { return map[string]any{"uid": "_:" + key} }
	doc := func(key, title, by string, private bool, deleted string, comments ...string) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": id[key], "doc_title": title, "doc_private": private,
			"doc_deleted_at": deleted, "doc_owner": ref(by), "doc_created_by": ref(by)}
		var cs []map[string]any
		for _, c := range comments {
			cs = append(cs, ref(c))
		}
		if len(cs) > 0 {
			n["doc_comments"] = cs
		}
		return n
	}
	comment := func(key, by, on string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Comment", "comment_uuid": id[key], "comment_text": "On the budget",
			"comment_deleted_at": zero, "comment_by": ref(by), "comment_doc": ref(on)}
	}
	nodes = append(nodes,
		doc("plan", "Budget plan", "cy", false, zero, "c1", "c2"),
		comment("c1", "ben", "plan"), comment("c2", "bot", "plan"),
		doc("secret", "Budget secret", "ben", true, zero, "c3"),
		comment("c3", "gil", "secret"),
		doc("gonedoc", "Old budget", "cy", false, "2026-10-01T00:00:00Z"),
		doc("evedoc", "Budget notes", "eve", false, zero),
		doc("botdoc", "Budget digest", "bot", false, zero),
		doc("extdoc", "Budget from outside", "ext", false, zero, "c4"),
		comment("c4", "ext", "extdoc"),
		doc("olddoc", "Budget archive", "old", false, zero),
		map[string]any{"uid": "_:proj", "dgraph.type": "Project", "project_uuid": id["proj"], "project_name": "P", "project_deleted_at": zero,
			"project_members": []map[string]any{ref("ana"), ref("cy"), ref("dee")}, "project_admins": []map[string]any{ref("cy")},
			"project_tasks": []map[string]any{ref("task")}},
		map[string]any{"uid": "_:task", "dgraph.type": "Task", "task_uuid": id["task"], "task_name": "Close the budget", "task_status": "todo",
			"task_deleted_at": zero, "task_project": ref("proj"), "task_created_by": ref("cy"), "task_assignee": ref("dee")},
		map[string]any{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": id["launch"], "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": []map[string]any{ref("ana"), ref("fay")}},
		map[string]any{"uid": "_:exec", "dgraph.type": "Channel", "ch_uuid": id["exec"], "ch_name": "exec", "ch_private": true,
			"ch_deleted_at": zero, "ch_members": []map[string]any{ref("fay")}, "ch_posts": []map[string]any{ref("m1")}},
		map[string]any{"uid": "_:m1", "dgraph.type": "Post", "post_uuid": id["m1"], "post_text": "<p>The budget is tight</p>", "post_deleted_at": zero,
			"post_created_at": "2026-10-10T10:00:00Z", "post_by": ref("fay"), "post_channel": ref("exec")},
	)
	uids := dg.Mutate(t, nodes)

	// The index, as stale as it can be: every doc public, the post findable.
	index := func(idx, docID string, body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/%s/_doc/%s?refresh=true", search.URL, idx, docID), bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("index %s/%s: %d", idx, docID, resp.StatusCode)
		}
	}
	for _, k := range []string{"plan", "secret", "gonedoc", "evedoc", "botdoc", "extdoc", "olddoc"} {
		index("docs", id[k], map[string]any{"doc_uuid": id[k], "doc_title": "Budget " + k, "doc_body": "the budget", "doc_private": false})
	}
	index("tasks", id["task"], map[string]any{"task_id": id["task"], "task_name": "Close the budget", "task_project_id": id["proj"]})
	index("posts", id["m1"], map[string]any{"post_id": id["m1"], "post_body": "The budget is tight", "post_ch_id": id["exec"]})

	name := map[string]string{}
	for k, v := range people {
		name[v.String()] = k
	}
	for k, v := range id {
		name[v] = k
	}
	type reply struct {
		code int
		raw  string
		body map[string]any
	}
	call := func(who string, handler http.HandlerFunc, method, query string, body string) reply {
		t.Helper()
		req := httptest.NewRequest(method, "/people/who-knows?"+query, strings.NewReader(body))
		c := ctx
		person := who
		if who == "tok" {
			person, c = "ana", helpers.WithActsThroughToken(c)
		}
		if who != "nobody" {
			var u userModels.UserInfo
			u.UserPostgresInfo.Id = people[person]
			u.UserPostgresInfo.EmailID = people[person].String() + "@example.test"
			u.UserPostgresInfo.IsAdmin = true // an admin asking changes nothing; through a token, still a member
			u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[person], Uuid: people[person].String(), UserName: person,
				Channels: []*dgraphStruct.DgraphChannel{{Uuid: id["launch"]}, {Uuid: id["exec"]}}, // exec: a stale claim
				Projects: []*dgraphStruct.DgraphProject{{Uuid: id["proj"]}}}
			c = context.WithValue(c, helpers.UserInfoContextKey, u)
		}
		rec := httptest.NewRecorder()
		handler(rec, req.WithContext(c))
		out := reply{code: rec.Code, raw: rec.Body.String()}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out
	}
	ask := func(who string, extra ...string) reply {
		q := url.Values{"q": {"budget"}}
		for i := 0; i+1 < len(extra); i += 2 {
			q.Set(extra[i], extra[i+1])
		}
		return call(who, whoKnowsController.GetWhoKnows, "GET", q.Encode(), "")
	}
	// lines is an answer as "name wrote/created/posted/assigned/commented:
	// evidence,…", in its order.
	lines := func(r reply) []string {
		data, _ := r.body["data"].(map[string]any)
		var out []string
		ps, _ := data["people"].([]any)
		for _, p := range ps {
			pm := p.(map[string]any)
			var ev []string
			for _, e := range pm["evidence"].([]any) {
				em := e.(map[string]any)
				ev = append(ev, em["role"].(string)+">"+name[em["id"].(string)])
			}
			sort.Strings(ev)
			out = append(out, fmt.Sprintf("%s %v/%v/%v/%v/%v: %s", name[pm["id"].(string)], pm["wrote"], pm["created"], pm["posted"], pm["assigned"], pm["commented"], strings.Join(ev, ",")))
		}
		return out
	}
	mustNotName := func(t *testing.T, r reply, keys ...string) {
		t.Helper()
		for _, k := range keys {
			v := id[k]
			if p, ok := people[k]; ok {
				v = p.String()
			}
			if strings.Contains(r.raw, v) {
				t.Errorf("the answer names %s: %s", k, r.raw)
			}
		}
	}

	view := []string{
		"cy 1/1/0/0/0: wrote>plan,wrote>task",
		"dee 0/0/0/1/0: assigned>task",
		"ben 0/0/0/0/1: commented>plan",
	}

	t.Run("evidence only from what the asker can read", func(t *testing.T) {
		r := ask("ana")
		if r.code != 200 {
			t.Fatalf("%d %s", r.code, r.raw)
		}
		if got := lines(r); strings.Join(got, "\n") != strings.Join(view, "\n") {
			t.Errorf("ana hears\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(view, "\n"))
		}
		// The secret, its commenter, the private channel's post and its
		// author, and the deleted doc are neither counted nor named.
		mustNotName(t, r, "secret", "gil", "m1", "fay", "gonedoc", "exec")
		data := r.body["data"].(map[string]any)
		// plan, evedoc, botdoc, extdoc, olddoc and the task: never the
		// secret, the post or the deleted doc, nor a word about them.
		if c := data["considered"].(float64); c != 6 {
			t.Errorf("considered %v, want 6", c)
		}
		if data["you_hidden"].(bool) {
			t.Error("ana is told she's hidden")
		}
	})

	t.Run("bots, external accounts and deleted people are never named", func(t *testing.T) {
		mustNotName(t, ask("ana"), "bot", "ext", "old")
	})

	t.Run("someone who left themselves out is never named", func(t *testing.T) {
		mustNotName(t, ask("ana"), "eve")
		// Eve was out from the start; ben now leaves too.
		if r := call("ben", whoKnowsController.SetMyWhoKnows, "PUT", "", `{"listed":false}`); r.code != 200 {
			t.Fatalf("%d %s", r.code, r.raw)
		}
		r := ask("ana")
		mustNotName(t, r, "ben")
		if got := lines(r); len(got) != 2 {
			t.Errorf("with ben out: %v", got)
		}
		me := call("ben", whoKnowsController.GetMyWhoKnows, "GET", "", "")
		if d, _ := me.body["data"].(map[string]any); d["listed"] != false {
			t.Errorf("ben's own setting reads %s", me.raw)
		}
		if r := ask("ben"); !r.body["data"].(map[string]any)["you_hidden"].(bool) {
			t.Errorf("ben isn't told he's hidden: %s", r.raw)
		}
		if r := call("ben", whoKnowsController.SetMyWhoKnows, "PUT", "", `{"listed":true}`); r.code != 200 {
			t.Fatal(r.raw)
		}
		if got := lines(ask("ana")); strings.Join(got, "\n") != strings.Join(view, "\n") {
			t.Errorf("ben back in: %v", got)
		}
	})

	t.Run("a token reads as the member it belongs to", func(t *testing.T) {
		if got := lines(ask("tok")); strings.Join(got, "\n") != strings.Join(view, "\n") {
			t.Errorf("the token hears %v", got)
		}
	})

	t.Run("caps and refusals", func(t *testing.T) {
		if got := lines(ask("ana", "limit", "1")); len(got) != 1 || !strings.HasPrefix(got[0], "cy ") {
			t.Errorf("limit 1: %v", got)
		}
		// Past the most is the most (helpers.SizeParam): all three here.
		if r := ask("ana", "limit", "500"); r.code != 200 || len(lines(r)) != 3 {
			t.Errorf("limit 500: %d %v", r.code, lines(r))
		}
		if r := call("ana", whoKnowsController.GetWhoKnows, "GET", "q=b", ""); r.code != 400 {
			t.Errorf("one character: %d", r.code)
		}
		if r := ask("nobody"); r.code != 401 {
			t.Errorf("nobody: %d", r.code)
		}
	})
}
