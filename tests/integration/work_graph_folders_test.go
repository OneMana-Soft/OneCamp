//go:build integration
// +build integration

package integration_test

// The work graph (DG5) meets the doc tree's folders, files and links (docs
// F1, DGF1), through its handlers and Ask AI's one-hop read, against real
// Postgres 12 and Dgraph with the production schema.
//
//   - olu made everything but two pages: a workspace folder, Finance (moss),
//     with a file in it (Q3 budget.xlsx), a page in it (Budget notes) and a
//     link under that page (Forecast, sky); a private folder, Vault, with a
//     private file in it (Salaries.xlsx); project P and its task, which
//     links the salaries file;
//   - kay made Comp bands and lee made Offer letter, two private pages in
//     the Vault, each shared with pat and olu. Nothing else joins them: no
//     maker, owner, link or message in common, only the Vault;
//   - pat is a member, in P, who can open neither the Vault nor the file.
//
// The rules held: a folder, a file and a link say what they are and their
// colour, under Docs, and the doc tree's edge says what it joins (in_folder,
// contains, holds); a file in a private folder is invisible with its whole
// path, through the panel, paths and Ask AI's one hop, and so is the folder
// from inside it. olu, who can open all of it, sees the same graph connect,
// so the rules are tested on paths that are there.
//
// Run: integ_rel.sh <wt> <log> '^TestWorkGraphFolders$'

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workGraphBusiness "github.com/akashc777/OneCamp/business/WorkGraph"
	workGraphController "github.com/akashc777/OneCamp/controllers/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWorkGraphFolders(t *testing.T) {
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
	for _, n := range []string{"olu", "pat", "kay", "lee"} {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n, "user_deleted_at": zero})
	}
	id := map[string]string{}
	for _, k := range []string{"fin", "q3", "notes", "link1", "vault", "sal", "comp", "offer", "proj", "task2"} {
		id[k] = uuid.NewString()
	}
	ref := func(key string) map[string]any { return map[string]any{"uid": "_:" + key} }
	// doc is a doc node made and owned by by; extra sets its kind, colour,
	// parent and grants.
	doc := func(key, title, by string, private bool, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": id[key], "doc_title": title, "doc_private": private,
			"doc_deleted_at": zero, "doc_owner": ref(by), "doc_created_by": ref(by)}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	sharedWithPatAndOlu := []map[string]any{ref("pat"), ref("olu")}
	nodes = append(nodes,
		doc("fin", "Finance", "olu", false, map[string]any{"doc_node_kind": "folder", "doc_color": "moss"}),
		doc("q3", "Q3 budget.xlsx", "olu", false, map[string]any{"doc_node_kind": "file", "doc_parent": ref("fin")}),
		doc("notes", "Budget notes", "olu", false, map[string]any{"doc_parent": ref("fin")}),
		doc("link1", "Forecast", "olu", false, map[string]any{"doc_node_kind": "link", "doc_color": "sky",
			"doc_link_url": "https://example.com/forecast", "doc_parent": ref("notes")}),
		doc("vault", "Vault", "olu", true, map[string]any{"doc_node_kind": "folder"}),
		doc("sal", "Salaries.xlsx", "olu", true, map[string]any{"doc_node_kind": "file", "doc_parent": ref("vault")}),
		doc("comp", "Comp bands", "kay", true, map[string]any{"doc_parent": ref("vault"), "doc_reading_users": sharedWithPatAndOlu}),
		doc("offer", "Offer letter", "lee", true, map[string]any{"doc_parent": ref("vault"), "doc_reading_users": sharedWithPatAndOlu}),
		map[string]any{"uid": "_:proj", "dgraph.type": "Project", "project_uuid": id["proj"], "project_name": "P", "project_deleted_at": zero,
			"project_members": []map[string]any{ref("olu"), ref("pat")}, "project_admins": []map[string]any{ref("olu")},
			"project_tasks": []map[string]any{ref("task2")}},
		map[string]any{"uid": "_:task2", "dgraph.type": "Task", "task_uuid": id["task2"], "task_name": "Pay the team", "task_status": "todo",
			"task_deleted_at": zero, "task_project": ref("proj"), "task_created_by": ref("olu"),
			"linked_docs": []map[string]any{ref("sal")}},
	)
	uids := dg.Mutate(t, nodes)

	name := map[string]string{}
	for k, v := range id {
		name[v] = k
	}
	for k, v := range people {
		name[v.String()] = k
	}
	userOf := func(who string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = people[who].String() + "@example.test"
		u.UserPostgresInfo.IsAdmin = who == "olu"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		return u
	}
	type reply struct {
		code int
		raw  string
		body map[string]any
	}
	call := func(who string, handler http.HandlerFunc, q url.Values) reply {
		t.Helper()
		req := httptest.NewRequest("GET", "/graph?"+q.Encode(), nil)
		c := context.WithValue(ctx, helpers.UserInfoContextKey, userOf(who))
		rec := httptest.NewRecorder()
		handler(rec, req.WithContext(c))
		out := reply{code: rec.Code, raw: rec.Body.String()}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out
	}
	connected := func(who, kind, key string, depth int) reply {
		t.Helper()
		r := call(who, workGraphController.GetConnected, url.Values{"kind": {kind}, "uuid": {id[key]}, "depth": {fmt.Sprint(depth)}})
		if r.code != 200 {
			t.Fatalf("%s's panel for %s: %d %s", who, key, r.code, r.raw)
		}
		return r
	}
	path := func(who, from, to string) reply {
		return call(who, workGraphController.GetPath, url.Values{"from_kind": {"doc"}, "from": {id[from]}, "to_kind": {"doc"}, "to": {id[to]}})
	}
	hopsOf := func(path []any) string {
		var hops []string
		for _, h := range path {
			hm := h.(map[string]any)
			hops = append(hops, hm["rel"].(string)+">"+name[hm["id"].(string)])
		}
		return strings.Join(hops, " ")
	}
	type row struct {
		via  string
		data map[string]any
	}
	// rows is a Connected answer's things of kind as name: the path to it
	// and its data.
	rows := func(r reply, kind string) map[string]row {
		out := map[string]row{}
		data, _ := r.body["data"].(map[string]any)
		groups, _ := data["groups"].([]any)
		for _, g := range groups {
			gm := g.(map[string]any)
			if gm["kind"] != kind {
				continue
			}
			for _, it := range gm["items"].([]any) {
				im := it.(map[string]any)
				d, _ := im["data"].(map[string]any)
				out[name[im["id"].(string)]] = row{via: hopsOf(im["path"].([]any)), data: d}
			}
		}
		return out
	}
	vias := func(rs map[string]row) []string {
		var out []string
		for k, r := range rs {
			out = append(out, k+" via "+r.via)
		}
		sort.Strings(out)
		return out
	}
	// mustNotName: an answer never names these things, by id or by title.
	// One exception, which is the chips' (C1) and not the graph's: a page
	// the reader may open says its parent's uuid (data.parent_id, which the
	// subpage block compares with the page it sits in), so a page inside
	// the Vault says the Vault's bare uuid there, and only there: never its
	// title, and never as a thing of the answer. Any read of that uuid is
	// "not found" to pat.
	titles := map[string]string{"vault": "Vault", "sal": "Salaries.xlsx", "offer": "Offer letter", "comp": "Comp bands"}
	mustNotName := func(t *testing.T, raw string, keys ...string) {
		t.Helper()
		for _, k := range keys {
			v := id[k]
			if p, ok := people[k]; ok {
				v = p.String()
			}
			ownParent := regexp.MustCompile(`"parent_id":\s*"` + regexp.QuoteMeta(v) + `"`)
			if rest := ownParent.ReplaceAllString(raw, ""); strings.Contains(rest, v) {
				t.Errorf("the answer names %s (%s): %s", k, v, raw)
			}
			if title := titles[k]; title != "" && strings.Contains(raw, title) {
				t.Errorf("the answer names %s by its title: %s", k, raw)
			}
		}
	}
	pathsOf := func(r reply) []string {
		data, _ := r.body["data"].(map[string]any)
		var out []string
		ps, _ := data["paths"].([]any)
		for _, p := range ps {
			out = append(out, hopsOf(p.(map[string]any)["hops"].([]any)))
		}
		return out
	}

	// Paths are kept shortest first, then by the allow-list's order (the
	// tree's edge before a maker's), so where a thing is reached both ways
	// the tree's way is the one shown. olu made the Vault too: pat never
	// sees it through olu either.
	t.Run("a folder, a file and a link say what they are, under Docs", func(t *testing.T) {
		r := connected("pat", "doc", "fin", 2)
		root := r.body["data"].(map[string]any)["root"].(map[string]any)["data"].(map[string]any)
		if root["node_kind"] != "folder" || root["color"] != "moss" {
			t.Errorf("the folder asked about says %v", root)
		}
		docs := rows(r, "doc")
		want := []string{"link1 via contains>notes holds>link1", "notes via contains>notes", "q3 via contains>q3"}
		if got := vias(docs); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("pat's Docs under Finance:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		mustNotName(t, r.raw, "vault", "sal")
		if d := docs["q3"].data; d["node_kind"] != "file" || d["color"] != nil {
			t.Errorf("the file's row: %v", d)
		}
		if d := docs["link1"].data; d["node_kind"] != "link" || d["color"] != "sky" {
			t.Errorf("the link's row: %v", d)
		}
		if d := docs["notes"].data; d["node_kind"] != nil {
			t.Errorf("a page's row says a kind: %v", d)
		}
		// A link's address isn't the graph's to give: its own page opens it.
		if strings.Contains(r.raw, "example.com") || strings.Contains(r.raw, "link_url") {
			t.Errorf("the panel carries the link's address: %s", r.raw)
		}
	})

	t.Run("a file's panel: its folder, and what else the folder holds", func(t *testing.T) {
		r := connected("pat", "doc", "q3", 2)
		docs := rows(r, "doc")
		// The link, two steps away only through olu who made both, may show
		// that way; the folder and its page show by the tree.
		for k, want := range map[string]string{"fin": "in_folder>fin", "notes": "in_folder>fin contains>notes"} {
			if docs[k].via != want {
				t.Errorf("pat reaches %s from the file via %q, want %q (%v)", k, docs[k].via, want, vias(docs))
			}
		}
		mustNotName(t, r.raw, "vault", "sal")
		if d := docs["fin"].data; d["node_kind"] != "folder" || d["color"] != "moss" {
			t.Errorf("the folder's row: %v", d)
		}
		// From the page, the link it holds and the folder it's in; pages
		// under pages would still read parent and subpage (unit tests).
		if got := vias(rows(connected("pat", "doc", "notes", 1), "doc")); strings.Join(got, ",") != "fin via in_folder>fin,link1 via holds>link1" {
			t.Errorf("pat's Docs around the page: %v", got)
		}
	})

	t.Run("the private folder never shows from inside it, nor anything reached only through it", func(t *testing.T) {
		r := connected("pat", "doc", "comp", 2)
		mustNotName(t, r.raw, "vault", "sal", "offer", "olu")
		if got := vias(rows(r, "doc")); len(got) != 0 {
			t.Errorf("pat's Docs around Comp bands: %v", got)
		}
		// olu, who can open the Vault, sees the same graph connect.
		got := vias(rows(connected("olu", "doc", "comp", 2), "doc"))
		for _, want := range []string{"vault via in_folder>vault", "offer via in_folder>vault contains>offer", "sal via in_folder>vault contains>sal"} {
			if !strings.Contains(strings.Join(got, "\n"), want) {
				t.Errorf("olu's Docs around Comp bands lack %q: %v", want, got)
			}
		}
	})

	t.Run("a task that links the hidden file shows neither it nor its folder", func(t *testing.T) {
		r := connected("pat", "task", "task2", 2)
		mustNotName(t, r.raw, "sal", "vault", "comp", "offer")
		got := vias(rows(connected("olu", "task", "task2", 2), "doc"))
		for _, want := range []string{"sal via links_to>sal", "vault via links_to>sal in_folder>vault"} {
			if !strings.Contains(strings.Join(got, "\n"), want) {
				t.Errorf("olu's Docs around the task lack %q: %v", want, got)
			}
		}
	})

	t.Run("two pages joined only through the private folder: no path, nothing named", func(t *testing.T) {
		r := path("pat", "comp", "offer")
		if r.code != 200 {
			t.Fatalf("pat, Comp bands to Offer letter: %d %s", r.code, r.raw)
		}
		if got := pathsOf(r); len(got) != 0 {
			t.Errorf("pat is given a way: %v", got)
		}
		mustNotName(t, r.raw, "vault", "sal", "olu")
		// olu gets a way, and every way olu is given goes through the Vault:
		// it is the only thing joining them.
		r = path("olu", "comp", "offer")
		got := pathsOf(r)
		if len(got) == 0 {
			t.Fatalf("olu, Comp bands to Offer letter: no way: %s", r.raw)
		}
		for _, p := range got {
			if !strings.Contains(p, "in_folder>vault") {
				t.Errorf("olu's way doesn't go through the Vault: %s", p)
			}
		}
	})

	t.Run("Ask AI's one hop names the folder's edge, and keeps the private folder out", func(t *testing.T) {
		readers := []accessBusiness.Subject{accessBusiness.SubjectFor(ctx, ptr(userOf("pat")))}
		seeds := []workGraphBusiness.Ref{{Kind: "doc", ID: id["q3"]}, {Kind: "doc", ID: id["fin"]}, {Kind: "doc", ID: id["comp"]}, {Kind: "task", ID: id["task2"]}}
		links, err := workGraphBusiness.ExpandFor(ctx, readers, seeds, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		raw, _ := json.Marshal(links)
		for _, l := range links {
			got = append(got, name[l.From.ID]+" "+l.Rel+" "+name[l.To.ID])
			if l.To.ID == id["fin"] && (l.To.NodeKind() != "folder" || l.To.Data["color"] != "moss") {
				t.Errorf("the folder's end of a link: %+v", l.To)
			}
		}
		for _, want := range []string{"q3 in_folder fin", "fin contains q3", "fin contains notes"} {
			if !strings.Contains(strings.Join(got, "\n"), want) {
				t.Errorf("Ask AI's links lack %q: %v", want, got)
			}
		}
		mustNotName(t, string(raw), "vault", "sal", "offer")
	})
}
