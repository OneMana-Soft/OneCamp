//go:build integration
// +build integration

package integration_test

// The owner's page-deletion rule (15 Oct 2026, "protect what they can't
// see"), through the handlers, against real Postgres 12 and Dgraph: a delete
// takes only the pages under it the person can open, and the rest move up to
// its parent with their own sharing; a delete for good removes only what the
// person has full access to, the rest staying in the trash, moved up the same
// way, an admin's included; a restore brings back what its delete took and
// leaves what moved where it went; and someone who can open everything still
// takes it all. The answers count the pages that stayed where the action
// reached them (kept), never what is under them, which the person can't see.
//
// Run: go test -tags=integration ./tests/integration/ -run TestADeleteProtectsWhatThePersonCantSee -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// eventLog keeps what the code under test emitted.
type eventLog struct {
	mu     sync.Mutex
	events []resourceEventBusiness.Event
}

func (l *eventLog) Emit(_ context.Context, e resourceEventBusiness.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// of is the events of one kind about one doc.
func (l *eventLog) of(kind resourceEventBusiness.Kind, docUUID string) []resourceEventBusiness.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []resourceEventBusiness.Event
	for _, e := range l.events {
		if e.Kind == kind && e.ResourceUUID == docUUID {
			out = append(out, e)
		}
	}
	return out
}

func TestADeleteProtectsWhatThePersonCantSee(t *testing.T) {
	ctx := context.Background()
	// Search answers every write; it is closed, and the client put back, only
	// after the harness has waited for the work the test handed off (cleanups
	// run last first, and the harness's are registered after this one).
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	wasSearch := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() {
		opensearchInit.OpenSearchClient = wasSearch
		search.Close()
	})
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	events := &eventLog{}
	wasBus := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = events
	t.Cleanup(func() { resourceEventBusiness.Bus = wasBus })

	// Dee is a workspace admin; the others are members.
	people := map[string]uuid.UUID{"ana": uuid.New(), "ben": uuid.New(), "cy": uuid.New(), "dee": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	uids := dg.Mutate(t, nodes)
	as := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserPostgresInfo.EmailID = people[name].String() + "@example.test"
		u.UserPostgresInfo.IsAdmin = name == "dee"
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
	create := func(title, parent string) string {
		t.Helper()
		a := call("ana", "POST", "/doc/createDoc", docController.CreateDocInTree, map[string]any{"doc_title": title, "parent_uuid": parent, "doc_private": false})
		if a.code != 200 {
			t.Fatalf("making %q under %q: %d %v", title, parent, a.code, a.body)
		}
		return str(a.data()["doc_uuid"])
	}
	// handOver makes a page Cy's and private, shared with nobody but the people
	// named, at the level the predicate gives them: what it shared as Ana's
	// page (its maker, and its parent's sharing copied when it was made) is
	// taken off first.
	handOver := func(id string, shares map[string]string) {
		t.Helper()
		uid := uidOf(t, dg, id)
		var drop strings.Builder
		for _, pred := range []string{"doc_reading_users", "doc_commenting_users", "doc_editing_users", "doc_full_access_users"} {
			fmt.Fprintf(&drop, "<%s> <%s> * .\n", uid, pred)
		}
		if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{DelNquads: []byte(drop.String()), CommitNow: true}); err != nil {
			t.Fatal(err)
		}
		m := map[string]any{"uid": uid, "doc_private": true, "doc_owner": map[string]any{"uid": uids["cy"]}}
		for who, pred := range shares {
			m[pred] = []map[string]any{{"uid": uids[who]}}
		}
		dg.Mutate(t, m)
	}
	type facts struct {
		Parent *struct {
			DocUUID string `json:"doc_uuid"`
		} `json:"doc_parent"`
		DeletedAt string                 `json:"doc_deleted_at"`
		Batch     string                 `json:"doc_deleted_batch"`
		Private   bool                   `json:"doc_private"`
		Owner     *struct{ UID string }  `json:"doc_owner"`
		Editing   []struct{ UID string } `json:"doc_editing_users"`
		Reading   []struct{ UID string } `json:"doc_reading_users"`
	}
	read := func(id string) (facts, bool) {
		t.Helper()
		var out struct {
			Q []facts `json:"q"`
		}
		raw := dg.Query(t, `{ q(func: eq(doc_uuid, "`+id+`")) {
			doc_parent { doc_uuid } doc_deleted_at doc_deleted_batch doc_private
			doc_owner { uid } doc_editing_users { uid } doc_reading_users { uid } } }`)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Q) == 0 {
			return facts{}, false
		}
		return out.Q[0], true
	}
	parentOf := func(f facts) string {
		if f.Parent == nil {
			return ""
		}
		return f.Parent.DocUUID
	}
	live := func(f facts) bool { return f.DeletedAt == "" || strings.HasPrefix(f.DeletedAt, "0001-") }
	del := func(who, id string) answer {
		return call(who, "POST", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": id})
	}
	purge := func(who, id string) answer {
		return call(who, "POST", "/doc/purge", docController.PurgeDocForGood, map[string]any{"doc_uuid": id})
	}
	restore := func(who, id string) answer {
		return call(who, "POST", "/doc/restore", docController.RestoreDocFromTrash, map[string]any{"doc_uuid": id})
	}

	// R > P > {A, H > H1}: all Ana's, but H and H1 under it are Cy's and
	// private, H shared with Ben to edit. Ana can't open either.
	r := create("Handbook", "")
	p := create("Team pages", r)
	a := create("Agenda", p)
	h := create("Cy's notes", p)
	h1 := create("Cy's drafts", h)
	handOver(h, map[string]string{"ben": "doc_editing_users"})
	handOver(h1, nil)

	t.Run("a hidden child survives the delete and moves up to the deleted page's parent", func(t *testing.T) {
		got := del("ana", p)
		if got.code != 200 || num(got.data()["count"]) != 2 || num(got.data()["kept"]) != 1 {
			t.Fatalf("Ana deleting P: %d %v (want 2 in the trash, 1 kept: H, with H1 under it uncounted)", got.code, got.body)
		}
		for _, id := range []string{p, a} {
			if f, _ := read(id); live(f) {
				t.Fatalf("%s should be in the trash", id)
			}
		}
		fh, _ := read(h)
		if !live(fh) || parentOf(fh) != r {
			t.Fatalf("H: live %v under %q, want live under R %s", live(fh), parentOf(fh), r)
		}
		if fh1, _ := read(h1); !live(fh1) || parentOf(fh1) != h {
			t.Fatalf("H1 should stay live under H: %+v", fh1)
		}
		// One activity entry for the move, by Ana, from P to R; nothing deleted
		// of what stayed.
		moved := events.of(resourceEventBusiness.DocMoved, h)
		if len(moved) != 1 || moved[0].Data["parent"] != r || moved[0].Data["from_parent"] != p || moved[0].Actor.UserUUID != people["ana"].String() {
			t.Fatalf("H's move: %+v", moved)
		}
		if n := len(events.of(resourceEventBusiness.DocDeleted, h)) + len(events.of(resourceEventBusiness.DocDeleted, h1)); n != 0 {
			t.Fatalf("pages that stayed were recorded as deleted (%d)", n)
		}
		if len(events.of(resourceEventBusiness.DocMoved, h1)) != 0 {
			t.Fatal("H1 moved with H; only the top of what stays moves")
		}
	})

	t.Run("the page that moved keeps its own sharing", func(t *testing.T) {
		fh, _ := read(h)
		if !fh.Private || fh.Owner == nil || fh.Owner.UID != uids["cy"] || len(fh.Editing) != 1 || fh.Editing[0].UID != uids["ben"] {
			t.Fatalf("H's sharing changed with the move: %+v", fh)
		}
		if fh1, _ := read(h1); !fh1.Private || fh1.Owner == nil || fh1.Owner.UID != uids["cy"] || len(fh1.Editing)+len(fh1.Reading) != 0 {
			t.Fatalf("H1's sharing changed with the move: %+v", fh1)
		}
		// Cy finds it under R; Ana, who still can't open it, is told only that
		// one is there.
		kids := call("cy", "GET", "/doc/children?parent="+r, docController.GetDocChildren, nil)
		if kids.code != 200 || !strings.Contains(fmt.Sprint(kids.body), "Cy's notes") {
			t.Fatalf("Cy's view of R: %d %v", kids.code, kids.body)
		}
		mine := call("ana", "GET", "/doc/children?parent="+r, docController.GetDocChildren, nil)
		if raw, _ := json.Marshal(mine.body); bytes.Contains(raw, []byte("Cy's notes")) || num(mine.data()["hidden"]) != 1 {
			t.Fatalf("Ana's view of R: %s", raw)
		}
	})

	t.Run("restoring the parent brings back what it took and leaves what moved where it went", func(t *testing.T) {
		got := restore("ana", p)
		if got.code != 200 || num(got.data()["restored"]) != 2 || str(got.data()["parent_uuid"]) != r {
			t.Fatalf("Ana restoring P: %d %v", got.code, got.body)
		}
		for _, id := range []string{p, a} {
			if f, _ := read(id); !live(f) {
				t.Fatalf("%s should be back", id)
			}
		}
		if fa, _ := read(a); parentOf(fa) != p {
			t.Fatalf("A should be back under P: %+v", fa)
		}
		if fh, _ := read(h); !live(fh) || parentOf(fh) != r {
			t.Fatalf("H should stay under R, where it moved: %+v", fh)
		}
	})

	t.Run("a hidden child survives a delete for good too", func(t *testing.T) {
		if got := del("ana", p); got.code != 200 || num(got.data()["kept"]) != 0 {
			t.Fatalf("Ana deleting P again: %d %v", got.code, got.body)
		}
		got := purge("ana", p)
		if got.code != 200 || num(got.data()["purged"]) != 2 || num(got.data()["kept"]) != 0 {
			t.Fatalf("Ana deleting P for good: %d %v", got.code, got.body)
		}
		for _, id := range []string{p, a} {
			if _, there := read(id); there {
				t.Fatalf("%s should be gone for good", id)
			}
		}
		if fh, _ := read(h); !live(fh) || parentOf(fh) != r {
			t.Fatalf("H should be untouched under R: %+v", fh)
		}
	})

	t.Run("a delete for good removes only what the person has full access to", func(t *testing.T) {
		// R2 > P2 > S > S1: S and S1 are Cy's, private, shared with Ana to
		// edit, so Ana's delete takes them, but she has no full access to them.
		r2 := create("Projects", "")
		p2 := create("Launch", r2)
		s := create("Cy's launch plan", p2)
		s1 := create("Cy's budget", s)
		handOver(s, map[string]string{"ana": "doc_editing_users"})
		handOver(s1, map[string]string{"ana": "doc_editing_users"})
		if got := del("ana", p2); got.code != 200 || num(got.data()["count"]) != 3 || num(got.data()["kept"]) != 0 {
			t.Fatalf("Ana deleting P2: %d %v", got.code, got.body)
		}
		before, _ := read(s)
		got := purge("ana", p2)
		if got.code != 200 || num(got.data()["purged"]) != 1 || num(got.data()["kept"]) != 1 {
			t.Fatalf("Ana deleting P2 for good: %d %v (want 1 gone, 1 kept: S, with S1 under it uncounted)", got.code, got.body)
		}
		if _, there := read(p2); there {
			t.Fatal("P2 should be gone for good")
		}
		fs, _ := read(s)
		if live(fs) || parentOf(fs) != r2 || fs.Batch == "" || fs.Batch == before.Batch {
			t.Fatalf("S should stay in the trash, under R2, a delete of its own: %+v (was batch %s)", fs, before.Batch)
		}
		if fs1, _ := read(s1); live(fs1) || parentOf(fs1) != s || fs1.Batch != fs.Batch {
			t.Fatalf("S1 should stay in the trash under S, in S's delete: %+v", fs1)
		}
		if moved := events.of(resourceEventBusiness.DocMoved, s); len(moved) != 1 || moved[0].Data["parent"] != r2 || moved[0].Data["from_parent"] != p2 {
			t.Fatalf("S's move: %+v", moved)
		}
		// S is its own entry in Cy's trash, and Cy brings it back under R2.
		trash := call("cy", "GET", "/doc/trash", docController.GetDocTrash, nil)
		var entry map[string]any
		for _, it := range list(trash.data()["items"]) {
			if m := it.(map[string]any); str(m["doc_uuid"]) == s {
				entry = m
			}
		}
		if entry == nil || num(entry["subpages"]) != 1 {
			t.Fatalf("Cy's trash: %v", trash.body)
		}
		back := restore("cy", s)
		if back.code != 200 || num(back.data()["restored"]) != 2 || str(back.data()["parent_uuid"]) != r2 {
			t.Fatalf("Cy restoring S: %d %v", back.code, back.body)
		}
	})

	t.Run("an admin's delete for good leaves what they can't open", func(t *testing.T) {
		// R3 > P3 > Z: Z is Cy's, private, shared with Ana to edit, so Ana's
		// delete takes it. Dee, an admin, may delete P3 for good, but can't
		// open Z: it stays in the trash for its people, moved up.
		r3 := create("Clients", "")
		p3 := create("Acme", r3)
		z := create("Cy's call notes", p3)
		handOver(z, map[string]string{"ana": "doc_editing_users"})
		if got := del("ana", p3); got.code != 200 || num(got.data()["count"]) != 2 {
			t.Fatalf("Ana deleting P3: %d %v", got.code, got.body)
		}
		got := purge("dee", p3)
		if got.code != 200 || num(got.data()["purged"]) != 1 || num(got.data()["kept"]) != 1 {
			t.Fatalf("Dee deleting P3 for good: %d %v (want 1 gone, 1 kept)", got.code, got.body)
		}
		if _, there := read(p3); there {
			t.Fatal("P3 should be gone for good")
		}
		fz, there := read(z)
		if !there || live(fz) || parentOf(fz) != r3 || !fz.Private || fz.Owner == nil || fz.Owner.UID != uids["cy"] {
			t.Fatalf("Z should stay in the trash under R3, still Cy's and private: %v %+v", there, fz)
		}
	})

	t.Run("someone who can open everything still takes the whole subtree", func(t *testing.T) {
		top := create("Archive", "")
		mid := create("2025", top)
		leaf := create("Q4", mid)
		if got := del("ana", top); got.code != 200 || num(got.data()["count"]) != 3 || num(got.data()["kept"]) != 0 {
			t.Fatalf("Ana deleting a tree that is all hers: %d %v", got.code, got.body)
		}
		if got := purge("ana", top); got.code != 200 || num(got.data()["purged"]) != 3 || num(got.data()["kept"]) != 0 {
			t.Fatalf("Ana deleting it for good: %d %v", got.code, got.body)
		}
		for _, id := range []string{top, mid, leaf} {
			if _, there := read(id); there {
				t.Fatalf("%s should be gone for good", id)
			}
		}
	})
}
