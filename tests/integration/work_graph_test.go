//go:build integration
// +build integration

package integration_test

// The work graph (DG5) through its handlers, against real Postgres 12 and
// Dgraph with the production schema: what is connected to a thing and how
// two things connect, as five readers see it.
//
//   - olu owns everything: a workspace doc (Plan), a private doc (Secret)
//     that Plan links, a doc reached only through Secret (Brief), a private
//     super-node doc (Hub) that links 40 workspace docs, a deleted doc
//     (Gone) that Plan links, project P and its task, and the private
//     channel #exec;
//   - mia is a member: in P, assigned its task, in the public #launch;
//   - out is in the workspace and nothing else;
//   - tok is mia through an API token (a token reads as a member);
//   - nobody is a request with no one signed in.
//
// The rules held: a hidden or missing thing is the same "not found"; a path
// through anything hidden is dropped whole (no gap, no count); deleted
// things never appear; a super-node costs its cap and shows "more" only to
// someone who can see more.
//
// Run: integ_rel.sh <wt> <log> '^TestWorkGraph$'

import (
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

	workGraphController "github.com/akashc777/OneCamp/controllers/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWorkGraph(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	zero := "0001-01-01T00:00:00Z"
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range []string{"olu", "mia", "out"} {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n, "user_deleted_at": zero})
	}
	id := map[string]string{}
	for _, k := range []string{"plan", "secret", "brief", "hub", "gone", "team", "proj", "task", "launch", "exec", "m1", "m2"} {
		id[k] = uuid.NewString()
	}
	doc := func(key, title string, private bool, deleted string, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": id[key], "doc_title": title, "doc_private": private,
			"doc_deleted_at": deleted, "doc_owner": map[string]any{"uid": "_:olu"}, "doc_created_by": map[string]any{"uid": "_:olu"}}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	ref := func(key string) map[string]any { return map[string]any{"uid": "_:" + key} }
	// The hub's 40 workspace docs.
	var hubLinks []map[string]any
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("h%d", i)
		id[key] = uuid.NewString()
		nodes = append(nodes, doc(key, "Hub page "+key, false, zero, nil))
		hubLinks = append(hubLinks, ref(key))
	}
	nodes = append(nodes,
		doc("plan", "Plan", false, zero, map[string]any{"doc_mentions": []map[string]any{ref("secret"), ref("gone"), ref("hub")}}),
		doc("secret", "Secret", true, zero, map[string]any{"doc_mentions": []map[string]any{ref("brief")}}),
		doc("brief", "Brief", false, zero, nil),
		doc("hub", "Hub", true, zero, map[string]any{"doc_mentions": hubLinks}),
		doc("gone", "Gone", false, "2026-10-01T00:00:00Z", nil),
		map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": id["team"], "team_name": "Launch team", "team_deleted_at": zero,
			"team_members": []map[string]any{ref("olu"), ref("mia")}, "team_projects": []map[string]any{ref("proj")}},
		map[string]any{"uid": "_:proj", "dgraph.type": "Project", "project_uuid": id["proj"], "project_name": "P", "project_deleted_at": zero,
			"project_team": ref("team"), "project_members": []map[string]any{ref("olu"), ref("mia")}, "project_admins": []map[string]any{ref("olu")},
			"project_tasks": []map[string]any{ref("task")}},
		map[string]any{"uid": "_:task", "dgraph.type": "Task", "task_uuid": id["task"], "task_name": "Record the film", "task_status": "todo",
			"task_deleted_at": zero, "task_project": ref("proj"), "task_created_by": ref("olu"), "task_assignee": ref("mia"),
			"linked_docs": []map[string]any{ref("plan")}},
		map[string]any{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": id["launch"], "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": []map[string]any{ref("olu"), ref("mia")}, "ch_posts": []map[string]any{ref("m1")}},
		map[string]any{"uid": "_:exec", "dgraph.type": "Channel", "ch_uuid": id["exec"], "ch_name": "exec", "ch_private": true,
			"ch_deleted_at": zero, "ch_members": []map[string]any{ref("olu")}, "ch_posts": []map[string]any{ref("m2")}},
		map[string]any{"uid": "_:m1", "dgraph.type": "Post", "post_uuid": id["m1"], "post_text": "<p>The plan is up</p>", "post_deleted_at": zero,
			"post_created_at": "2026-10-10T10:00:00Z", "post_by": ref("olu"), "post_channel": ref("launch"), "post_doc": ref("plan")},
		map[string]any{"uid": "_:m2", "dgraph.type": "Post", "post_uuid": id["m2"], "post_text": "<p>Budget for the plan</p>", "post_deleted_at": zero,
			"post_created_at": "2026-10-11T10:00:00Z", "post_by": ref("olu"), "post_channel": ref("exec"), "post_doc": ref("plan")},
	)
	uids := dg.Mutate(t, nodes)

	name := map[string]string{}
	for k, v := range id {
		name[v] = k
	}
	for k, v := range people {
		name[v.String()] = k
	}
	type reply struct {
		code int
		raw  string
		body map[string]any
	}
	call := func(who string, handler http.HandlerFunc, q url.Values) reply {
		t.Helper()
		req := httptest.NewRequest("GET", "/graph?"+q.Encode(), nil)
		c := ctx
		person := who
		if who == "tok" {
			person = "mia"
			c = helpers.WithActsThroughToken(c)
		}
		if who != "nobody" {
			var u userModels.UserInfo
			u.UserPostgresInfo.Id = people[person]
			u.UserPostgresInfo.EmailID = people[person].String() + "@example.test"
			u.UserPostgresInfo.IsAdmin = person == "olu"
			u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[person], Uuid: people[person].String(), UserName: person}
			c = context.WithValue(c, helpers.UserInfoContextKey, u)
		}
		rec := httptest.NewRecorder()
		handler(rec, req.WithContext(c))
		out := reply{code: rec.Code, raw: rec.Body.String()}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out
	}
	connected := func(who, kind, key string, depth int) reply {
		return call(who, workGraphController.GetConnected, url.Values{"kind": {kind}, "uuid": {id[key]}, "depth": {fmt.Sprint(depth)}})
	}
	// items is a Connected answer as "kind name via rel>name rel>name", sorted.
	items := func(r reply) (out []string, more map[string]bool) {
		more = map[string]bool{}
		data, _ := r.body["data"].(map[string]any)
		groups, _ := data["groups"].([]any)
		for _, g := range groups {
			gm := g.(map[string]any)
			if m, _ := gm["more"].(bool); m {
				more[gm["kind"].(string)] = true
			}
			for _, it := range gm["items"].([]any) {
				im := it.(map[string]any)
				var hops []string
				for _, h := range im["path"].([]any) {
					hm := h.(map[string]any)
					hops = append(hops, hm["rel"].(string)+">"+name[hm["id"].(string)])
				}
				out = append(out, im["kind"].(string)+" "+name[im["id"].(string)]+" via "+strings.Join(hops, " "))
			}
		}
		sort.Strings(out)
		return out, more
	}
	mustNotName := func(t *testing.T, r reply, keys ...string) {
		t.Helper()
		for _, k := range keys {
			if strings.Contains(r.raw, id[k]) {
				t.Errorf("the answer names %s (%s): %s", k, id[k], r.raw)
			}
		}
	}

	memberView := []string{
		"channel launch via shared_in>m1 in_channel>launch",
		"message m1 via shared_in>m1",
		"person mia via linked_from>task assigned_to>mia",
		"person olu via created_by>olu",
		"project proj via linked_from>task in_project>proj",
		"task task via linked_from>task",
	}

	t.Run("a member sees what connects, and nothing through what they can't open", func(t *testing.T) {
		r := connected("mia", "doc", "plan", 2)
		if r.code != 200 {
			t.Fatalf("%d %s", r.code, r.raw)
		}
		got, more := items(r)
		if strings.Join(got, "\n") != strings.Join(memberView, "\n") {
			t.Errorf("mia sees\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(memberView, "\n"))
		}
		if len(more) != 0 {
			t.Errorf("mia is told there is more: %v", more)
		}
		mustNotName(t, r, "secret", "brief", "hub", "h0", "h39", "gone", "m2", "exec")
	})

	t.Run("a token reads as the member it belongs to", func(t *testing.T) {
		got, _ := items(connected("tok", "doc", "plan", 2))
		if strings.Join(got, "\n") != strings.Join(memberView, "\n") {
			t.Errorf("the token sees\n%s", strings.Join(got, "\n"))
		}
	})

	t.Run("the owner sees past the private docs, the hub capped, never the deleted doc", func(t *testing.T) {
		r := connected("olu", "doc", "plan", 2)
		got, more := items(r)
		joined := strings.Join(got, "\n")
		for _, want := range []string{
			"doc secret via links_to>secret", "doc brief via links_to>secret links_to>brief", "doc hub via links_to>hub",
			"message m2 via shared_in>m2", "channel exec via shared_in>m2 in_channel>exec",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("olu doesn't see %q in\n%s", want, joined)
			}
		}
		// Two hops out, the hub gives its first Cap2 pages, no more.
		if n := strings.Count(joined, "links_to>hub links_to>h"); n != 4 {
			t.Errorf("olu sees %d of the hub's pages two hops out, want 4", n)
		}
		_ = more
		mustNotName(t, r, "gone")
	})

	t.Run("an outsider sees only what the workspace may", func(t *testing.T) {
		r := connected("out", "doc", "plan", 2)
		got, _ := items(r)
		want := []string{"channel launch via shared_in>m1 in_channel>launch", "message m1 via shared_in>m1", "person olu via created_by>olu"}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("out sees\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		mustNotName(t, r, "task", "proj", "secret", "brief", "hub", "m2", "exec", "team")
	})

	t.Run("hidden, missing and deleted things are the same not found", func(t *testing.T) {
		hidden := connected("out", "doc", "secret", 1)
		missing := call("out", workGraphController.GetConnected, url.Values{"kind": {"doc"}, "uuid": {uuid.NewString()}})
		deleted := connected("olu", "doc", "gone", 1)
		if hidden.code != 404 || missing.code != 404 || deleted.code != 404 || hidden.raw != missing.raw || deleted.raw != missing.raw {
			t.Errorf("hidden %d %s, missing %d %s, deleted %d %s", hidden.code, hidden.raw, missing.code, missing.raw, deleted.code, deleted.raw)
		}
		if r := connected("nobody", "doc", "plan", 1); r.code != 404 {
			t.Errorf("no one signed in: %d %s", r.code, r.raw)
		}
		if r := call("mia", workGraphController.GetConnected, url.Values{"kind": {"doc"}, "uuid": {uids["olu"]}}); r.code != 400 {
			t.Errorf("a graph uid as the id: %d %s", r.code, r.raw)
		}
	})

	t.Run("the private hub costs its cap and hides its pages from a member", func(t *testing.T) {
		r := call("mia", workGraphController.GetConnected, url.Values{"kind": {"doc"}, "uuid": {id["hub"]}, "depth": {"2"}})
		if r.code != 404 {
			t.Errorf("mia opens the hub's connections: %d", r.code)
		}
		r = call("olu", workGraphController.GetConnected, url.Values{"kind": {"doc"}, "uuid": {id["hub"]}, "kinds": {"doc"}})
		got, more := items(r)
		if len(got) != 12 || !more["doc"] {
			t.Errorf("olu's hub: %d docs, more %v", len(got), more)
		}
	})

	path := func(who, fk, from, tk, to string) reply {
		return call(who, workGraphController.GetPath, url.Values{"from_kind": {fk}, "from": {id[from]}, "to_kind": {tk}, "to": {id[to]}})
	}
	paths := func(r reply) []string {
		data, _ := r.body["data"].(map[string]any)
		var out []string
		ps, _ := data["paths"].([]any)
		for _, p := range ps {
			var hops []string
			for _, h := range p.(map[string]any)["hops"].([]any) {
				hm := h.(map[string]any)
				hops = append(hops, hm["rel"].(string)+">"+name[hm["id"].(string)])
			}
			out = append(out, strings.Join(hops, " "))
		}
		return out
	}

	t.Run("paths go through what the reader can see, or not at all", func(t *testing.T) {
		if got := paths(path("mia", "doc", "plan", "project", "proj")); len(got) == 0 || got[0] != "linked_from>task in_project>proj" {
			t.Errorf("mia plan to P: %v", got)
		}
		// Brief is open to mia; olu owns it and Plan, so a way through olu is fair.
		r := path("mia", "doc", "plan", "doc", "brief")
		// A way through olu (who owns both) is a fair answer; one through
		// Secret is never given, nor is Secret named.
		if r.code != 200 {
			t.Errorf("mia plan to brief: %d %v", r.code, paths(r))
		}
		for _, p := range paths(r) {
			if strings.Contains(p, ">secret") {
				t.Errorf("mia is given a way through Secret: %s", p)
			}
		}
		mustNotName(t, r, "secret")
		// olu reaches Brief two ways of the same length (through Secret, or
		// through olu, who owns both), and Dgraph's k-shortest search gives
		// either, or both: seen in two runs on 19 Oct as the path through olu
		// alone. What matters is that olu, who may open Secret, gets a way.
		if got := paths(path("olu", "doc", "plan", "doc", "brief")); len(got) == 0 {
			t.Errorf("olu plan to brief: no way")
		}
		// From a channel, the search runs from the doc and comes back turned round.
		if got := paths(path("mia", "channel", "launch", "doc", "plan")); len(got) == 0 || got[0] != "message>m1 shares>plan" {
			t.Errorf("mia #launch to plan: %v", got)
		}
		if r := path("out", "doc", "plan", "project", "proj"); r.code != 404 {
			t.Errorf("out to a project they're not in: %d %s", r.code, r.raw)
		}
		// A channel's uuid named as a doc is a doc that isn't there.
		if r := path("mia", "doc", "plan", "doc", "exec"); r.code != 404 {
			t.Errorf("a channel's uuid as a doc: %d", r.code)
		}
	})
}
