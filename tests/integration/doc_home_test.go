//go:build integration
// +build integration

package integration_test

// The docs home (docs programme B5) through its handlers, against a real
// OpenSearch mapped as the server maps it, a real graph and a real Postgres:
//   - a word only in a doc's body finds it, with the passage it was found in;
//   - a private doc is found only by those it is shared with, and the count
//     is of what the person may open;
//   - a page above a result is named only for someone who may open it;
//   - a deleted doc is found by nobody;
//   - a template is written once: a doc made from one holds it once, and a
//     second "use this template" on the same doc is refused (409), however
//     many tabs ask (N37);
//   - a doc saved as a template starts a teammate's new doc with its body,
//     without its block ids (structure.md P1.8);
//   - only someone who may change a doc saves it as a template: a private
//     doc shared read-only is never published to the team.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocsHome -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	docHomeController "github.com/akashc777/OneCamp/controllers/DocHome"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// meetingNotes is OneCamp's own "Meeting notes" template.
const meetingNotes = "b5d0c000-0000-4000-8000-000000000001"

func TestDocsHome(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	// Every index, mapped as the server maps them at boot.
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	people := map[string]uuid.UUID{"owner": uuid.New(), "member": uuid.New(), "reader": uuid.New(), "editor": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	uids := dg.Mutate(t, nodes)
	as := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserPostgresInfo.EmailID = people[name].String() + "@example.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}
	call := func(who, method, target string, handler http.HandlerFunc, body any) answer {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, target, bytes.NewReader(raw))
		c := context.WithValue(ctx, helpers.UserInfoContextKey, as(who))
		req = req.WithContext(context.WithValue(c, chi.RouteCtxKey, chi.NewRouteContext()))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := answer{code: rec.Code}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out
	}
	create := func(who, title, parent string, private bool, template string) string {
		t.Helper()
		in := map[string]any{"doc_title": title, "doc_private": private}
		if parent != "" {
			in["parent_uuid"] = parent
		}
		if template != "" {
			in["template_id"] = template
		}
		a := call(who, "POST", "/doc/createDoc", docController.CreateDocInTree, in)
		if a.code != http.StatusOK {
			t.Fatalf("%s making %q: %d %v", who, title, a.code, a.body)
		}
		return str(a.data()["doc_uuid"])
	}
	// write gives a doc a body as a save would.
	write := func(id, html string) {
		t.Helper()
		if err := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{DocId: id, Body: &html}); err != nil {
			t.Fatal(err)
		}
	}
	type hit struct {
		title, titleMark string
		passages         []string
		path             []map[string]any
	}
	// find is what a search for text, as the person, answers: its docs by id
	// and its count.
	find := func(who, text string) (map[string]hit, int) {
		t.Helper()
		a := call(who, "GET", "/doc/search?q="+url.QueryEscape(text), docHomeController.SearchDocs, nil)
		if a.code != http.StatusOK {
			t.Fatalf("%s searching %q: %d %v", who, text, a.code, a.body)
		}
		out := map[string]hit{}
		for _, d := range list(a.data()["docs"]) {
			m := d.(map[string]any)
			h := hit{title: str(m["doc_title"]), titleMark: str(m["title_highlight"])}
			for _, p := range list(m["highlights"]) {
				h.passages = append(h.passages, str(p))
			}
			for _, c := range list(m["path"]) {
				h.path = append(h.path, c.(map[string]any))
			}
			out[str(m["doc_uuid"])] = h
		}
		return out, num(a.data()["count"])
	}
	// eventually waits for an answer that comes once the index has caught up
	// (entries are written in the background).
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("timed out waiting until %s", what)
	}

	handbook := create("owner", "Engineering handbook", "", false, "")
	write(handbook, "<h2>Shipping</h2><p>We deploy on Tuesdays, after the standup.</p>")
	salaries := create("owner", "Salaries review", "", true, "")
	write(salaries, "<p>The quarterly bonus pool is set in March.</p>")

	t.Run("a word only in the body finds the doc, with the passage it was found in", func(t *testing.T) {
		eventually("the member finds the handbook by a word in its body", func() bool {
			got, _ := find("member", "tuesdays")
			_, ok := got[handbook]
			return ok
		})
		got, count := find("member", "tuesdays")
		h := got[handbook]
		if count != len(got) || h.title != "Engineering handbook" {
			t.Fatalf("count %d for %d docs; %+v", count, len(got), h)
		}
		if len(h.passages) == 0 || !strings.Contains(h.passages[0], "<mark>Tuesdays</mark>") {
			t.Errorf("the passage doesn't mark the word: %v", h.passages)
		}
		if len(h.path) != 0 {
			t.Errorf("a page at the top has no path: %v", h.path)
		}
	})

	t.Run("a private doc is found only by those it is shared with", func(t *testing.T) {
		eventually("its owner finds the private doc", func() bool {
			got, _ := find("owner", "bonus")
			_, ok := got[salaries]
			return ok
		})
		if got, count := find("member", "bonus"); len(got) != 0 || count != 0 {
			t.Fatalf("a member finds, or counts, a private doc that isn't shared with them: %v %d", got, count)
		}
		if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: salaries, AddViewers: []string{people["reader"].String()}}, uids["owner"]); err != nil {
			t.Fatal(err)
		}
		eventually("the reader finds the doc shared with them", func() bool {
			got, _ := find("reader", "bonus")
			_, ok := got[salaries]
			return ok
		})
	})

	t.Run("a page above a result is named only for someone who may open it", func(t *testing.T) {
		board := create("owner", "Board notes", "", true, "")
		offsite := create("owner", "Offsite agenda", board, true, "")
		write(offsite, "<p>Hiking on the second morning.</p>")
		if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: offsite, AddViewers: []string{people["member"].String()}}, uids["owner"]); err != nil {
			t.Fatal(err)
		}
		eventually("the member finds the agenda shared with them", func() bool {
			got, _ := find("member", "hiking")
			_, ok := got[offsite]
			return ok
		})
		got, _ := find("member", "hiking")
		path := got[offsite].path
		if len(path) != 1 || path[0]["redacted"] != true || str(path[0]["doc_title"]) != "" || str(path[0]["doc_uuid"]) != "" {
			t.Fatalf("the page above, which the member can't open, is named: %v", path)
		}
		mine, _ := find("owner", "hiking")
		if p := mine[offsite].path; len(p) != 1 || str(p[0]["doc_title"]) != "Board notes" {
			t.Fatalf("its owner sees the page above by name: %v", p)
		}
	})

	t.Run("a search shorter than two letters is refused, saying why", func(t *testing.T) {
		a := call("member", "GET", "/doc/search?q=%20a%20", docHomeController.SearchDocs, nil)
		if a.code != http.StatusBadRequest || str(a.body["msg"]) != "Type at least 2 letters to search." {
			t.Fatalf("got %d %v", a.code, a.body)
		}
	})

	t.Run("a deleted doc is found by nobody", func(t *testing.T) {
		owner := as("owner")
		if _, err := docBusiness.TrashSubtree(ctx, handbook, owner.UserDgraphInfo.Uid, owner.UserDgraphInfo.Uuid, owner.UserPostgresInfo.EmailID); err != nil {
			t.Fatal(err)
		}
		eventually("its owner no longer finds the deleted handbook", func() bool {
			got, _ := find("owner", "tuesdays")
			_, ok := got[handbook]
			return !ok
		})
	})

	t.Run("a doc made from a template holds it once", func(t *testing.T) {
		id := create("owner", "Monday sync", "", true, meetingNotes)
		doc, err := docBusiness.GetDocByDocUUID(ctx, id, uids["owner"])
		if err != nil || doc == nil {
			t.Fatalf("reading it: %v", err)
		}
		if n := strings.Count(doc.Body, "Action items"); n != 1 {
			t.Fatalf("the template is in the doc %d times: %s", n, doc.Body)
		}
		if a := call("owner", "POST", "/doc/applyTemplate", docHomeController.ApplyTemplate, map[string]any{"doc_uuid": id, "template_id": meetingNotes}); a.code != http.StatusConflict || str(a.body["code"]) != "not_empty" {
			t.Fatalf("a template over a doc that has one: %d %v", a.code, a.body)
		}
	})

	t.Run("two tabs using a template on one empty doc never double it", func(t *testing.T) {
		id := create("owner", "", "", true, "")
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = call("owner", "POST", "/doc/applyTemplate", docHomeController.ApplyTemplate, map[string]any{"doc_uuid": id, "template_id": meetingNotes}).code
			}(i)
		}
		wg.Wait()
		ok := 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
			default:
				t.Fatalf("a tab got %d", c)
			}
		}
		if ok == 0 {
			t.Fatalf("neither tab's template went in: %v", codes)
		}
		doc, err := docBusiness.GetDocByDocUUID(ctx, id, uids["owner"])
		if err != nil || doc == nil {
			t.Fatalf("reading it: %v", err)
		}
		if n := strings.Count(doc.Body, "Action items"); n != 1 {
			t.Fatalf("the template is in the doc %d times: %s", n, doc.Body)
		}
		if a := call("owner", "POST", "/doc/applyTemplate", docHomeController.ApplyTemplate, map[string]any{"doc_uuid": id, "template_id": meetingNotes}); a.code != http.StatusConflict {
			t.Fatalf("a third try: %d %v", a.code, a.body)
		}
	})

	t.Run("a doc saved as a template starts a teammate's new doc with its body (P1.8)", func(t *testing.T) {
		src := create("owner", "Launch retro", "", true, "")
		write(src, `<h2 data-block-id="b-1">What went well</h2><p data-block-id="b-2">The demo landed.</p>`)
		a := call("owner", "POST", "/doc/saveAsTemplate", docHomeController.SaveAsTemplate, map[string]any{"doc_uuid": src, "name": "Retro, our way", "description": "After every launch."})
		if a.code != http.StatusCreated {
			t.Fatalf("saving it: %d %v", a.code, a.body)
		}
		tpl := a.data()
		payload := str(tpl["payload"])
		if str(tpl["kind"]) != "doc" || str(tpl["name"]) != "Retro, our way" || !strings.Contains(payload, `"preview"`) || strings.Contains(payload, `"body"`) {
			t.Fatalf("a saved template is listed with its preview, never its body: %v", tpl)
		}
		made := create("member", "", "", true, str(tpl["id"]))
		doc, err := docBusiness.GetDocByDocUUID(ctx, made, uids["member"])
		if err != nil || doc == nil {
			t.Fatalf("reading it: %v", err)
		}
		if strings.Count(doc.Body, "What went well") != 1 || !strings.Contains(doc.Body, "The demo landed.") {
			t.Fatalf("the new doc doesn't start with the template's body: %s", doc.Body)
		}
		if strings.Contains(doc.Body, "b-1") {
			t.Fatalf("a template carries the doc's block ids into the new doc: %s", doc.Body)
		}
		empty := create("owner", "", "", true, "")
		if a := call("owner", "POST", "/doc/saveAsTemplate", docHomeController.SaveAsTemplate, map[string]any{"doc_uuid": empty, "name": "Nothing"}); a.code != http.StatusUnprocessableEntity || str(a.body["code"]) != "empty" {
			t.Fatalf("an empty doc as a template: %d %v", a.code, a.body)
		}
		if a := call("member", "POST", "/doc/saveAsTemplate", docHomeController.SaveAsTemplate, map[string]any{"doc_uuid": empty, "name": "Not mine"}); a.code != http.StatusNotFound {
			t.Fatalf("a doc the person can't open: %d %v", a.code, a.body)
		}
	})

	t.Run("someone who may only read a doc can't put a template in it", func(t *testing.T) {
		id := create("owner", "", "", true, "")
		if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: id, AddViewers: []string{people["reader"].String()}}, uids["owner"]); err != nil {
			t.Fatal(err)
		}
		if a := call("reader", "POST", "/doc/applyTemplate", docHomeController.ApplyTemplate, map[string]any{"doc_uuid": id, "template_id": meetingNotes}); a.code != http.StatusForbidden {
			t.Fatalf("a reader: %d %v", a.code, a.body)
		}
		if a := call("member", "POST", "/doc/applyTemplate", docHomeController.ApplyTemplate, map[string]any{"doc_uuid": id, "template_id": meetingNotes}); a.code != http.StatusNotFound {
			t.Fatalf("someone it isn't shared with: %d %v", a.code, a.body)
		}
	})

	t.Run("someone who may only read a private doc can't publish it as a template", func(t *testing.T) {
		id := create("owner", "Salary bands", "", true, "")
		write(id, `<p>Only the founders read this.</p>`)
		if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: id, AddViewers: []string{people["reader"].String()}, AddEditors: []string{people["editor"].String()}}, uids["owner"]); err != nil {
			t.Fatal(err)
		}
		if a := call("reader", "POST", "/doc/saveAsTemplate", docHomeController.SaveAsTemplate, map[string]any{"doc_uuid": id, "name": "Leaked"}); a.code != http.StatusForbidden {
			t.Fatalf("a reader publishing it: %d %v", a.code, a.body)
		}
		a := call("editor", "POST", "/doc/saveAsTemplate", docHomeController.SaveAsTemplate, map[string]any{"doc_uuid": id, "name": "Bands, by an editor"})
		if a.code != http.StatusCreated || str(a.data()["name"]) != "Bands, by an editor" {
			t.Fatalf("an editor saving it: %d %v", a.code, a.body)
		}
	})
}
