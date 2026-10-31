//go:build integration
// +build integration

package integration_test

// Anchored threads (docs programme A8) against real stores: the threads
// migration on Postgres 12 (down, up, down and up, found by name so that its
// renumbering at merge keeps the test), a doc's comments in the graph written
// through the doc comment path and tagged with their thread, and the rules
// over HTTP: who may start, reply, resolve and read; flat comments from
// before threads read as page-level threads and become threads on a reply;
// a reattach is stored only when asked for.
//
// Run: go test -tags=integration ./tests/integration/ -run TestThreads -v

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	threadController "github.com/akashc777/OneCamp/controllers/Thread"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	threadComments "github.com/akashc777/OneCamp/models/dgraph/Thread"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

var threadsFile = regexp.MustCompile(`^[0-9]+_threads\.`)

// threadsMigration is the threads migration's file for dir ("up" or "down"),
// found by name: it is reserved as 210 and the integrator renumbers it at
// merge, which must not break this test.
func threadsMigration(t *testing.T, dir string) (name, sql string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	all, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_threads."+dir+".sql"))
	// Only <n>_threads: chat's own threads (<n>_chat_threads) end the same way.
	var matches []string
	for _, m := range all {
		if threadsFile.MatchString(filepath.Base(m)) {
			matches = append(matches, m)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want one threads %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(matches[0]), string(b)
}

// The migration runs down, up, down and up on Postgres 12, whatever number it
// has and whatever migrations come after it: its own files are run directly,
// not by stepping the migrator, which would undo the newest migration instead.
func TestThreadsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t) // every migration applied, this one among them
	upName, up := threadsMigration(t, "up")
	downName, down := threadsMigration(t, "down")
	makesNoIDs(t, upName, up)
	exists := func(table string) bool {
		var name *string
		if err := env.PG.QueryRow(`SELECT to_regclass($1)::text`, table).Scan(&name); err != nil {
			t.Fatal(err)
		}
		return name != nil
	}
	if !exists("threads") || !exists("thread_participants") {
		t.Fatalf("the migrations did not make the threads tables (%s)", upName)
	}
	for i, step := range []struct {
		name, sql string
		want      bool
	}{{downName, down, false}, {upName, up, true}, {downName, down, false}, {upName, up, true}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d, %s: %v", i+1, step.name, err)
		}
		if exists("threads") != step.want || exists("thread_participants") != step.want {
			t.Fatalf("step %d, %s: want the tables there = %v", i+1, step.name, step.want)
		}
	}
	// The checks hold: a range needs both ends, resolved needs a time.
	id, by := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO threads (id, resource_kind, resource_uuid, anchor_kind, anchor_start, created_by)
		VALUES ($1, 'doc', 'd', 'yrange', '\x01', $2)`, id, by); err == nil {
		t.Error("a range with one end was stored")
	}
	if _, err := env.PG.Exec(`INSERT INTO threads (id, resource_kind, resource_uuid, status, created_by)
		VALUES ($1, 'doc', 'd', 'resolved', $2)`, id, by); err == nil {
		t.Error("a resolved thread with no time was stored")
	}
}

func TestThreads(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
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

	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"owner", "ann", "bob", "viewer", "stranger"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	docID, trashed := uuid.NewString(), uuid.NewString()
	nodes = append(nodes,
		map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": docID, "doc_title": "Launch notes",
			"doc_private": true, "doc_public_comment": false, "doc_deleted_at": "0001-01-01T00:00:00Z",
			"doc_created_by":       map[string]any{"uid": "_:owner"},
			"doc_editing_users":    []map[string]any{{"uid": "_:owner"}},
			"doc_commenting_users": []map[string]any{{"uid": "_:ann"}, {"uid": "_:bob"}},
			"doc_reading_users":    []map[string]any{{"uid": "_:viewer"}}},
		map[string]any{"uid": "_:trashed", "dgraph.type": "Doc", "doc_uuid": trashed, "doc_title": "old",
			"doc_private": false, "doc_public_comment": true, "doc_deleted_at": "2026-01-01T00:00:00Z",
			"doc_created_by":    map[string]any{"uid": "_:owner"},
			"doc_editing_users": []map[string]any{{"uid": "_:owner"}}},
	)
	uids := dg.Mutate(t, nodes)
	userInfo := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}

	// The routes as router/docsThreads.go mounts them, signed in by a header.
	rt := chi.NewRouter()
	rt.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, userInfo(r.Header.Get("X-Who")))))
		})
	})
	rt.Get("/threads", threadController.ListThreads)
	rt.Post("/threads", threadController.CreateThread)
	rt.Get("/threads/{thread_id}", threadController.GetThread)
	rt.Post("/threads/{thread_id}/replies", threadController.ReplyToThread)
	rt.Post("/threads/{thread_id}/resolve", threadController.ResolveThread)
	rt.Post("/threads/{thread_id}/reopen", threadController.ReopenThread)
	rt.Post("/threads/{thread_id}/reanchor", threadController.ReanchorThread)
	rt.Post("/threads/{thread_id}/read", threadController.MarkThreadRead)

	call := func(who, method, path string, body any, out any) int {
		t.Helper()
		var rd *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("X-Who", who)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		if out != nil && rec.Code == http.StatusOK {
			var env struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("%s %s: %v %s", method, path, err, rec.Body.String())
			}
			if err := json.Unmarshal(env.Data, out); err != nil {
				t.Fatalf("%s %s: %v %s", method, path, err, env.Data)
			}
		}
		return rec.Code
	}
	list := func(who, status string) []threadBusiness.View {
		t.Helper()
		var out struct {
			Threads []threadBusiness.View `json:"threads"`
		}
		if code := call(who, http.MethodGet, "/threads?resource_kind=doc&resource_uuid="+docID+"&status="+status, nil, &out); code != http.StatusOK {
			t.Fatalf("%s listing: %d", who, code)
		}
		return out.Threads
	}

	// A flat comment, through the old route, before any thread.
	raw, _ := json.Marshal(docAdapter.CreateOrUpdateDocCommentInput{DocUuid: docID, CommentBody: "<p>old and flat</p>"})
	rec := httptest.NewRecorder()
	docController.CreateDocComment(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
		WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, userInfo("bob"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("the old comment route: %d %s", rec.Code, rec.Body.String())
	}
	var flat struct {
		Data struct {
			ID string `json:"comment_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &flat)

	start, end := []byte{1, 2, 3, 4}, []byte{5, 6, 7}
	anchor := map[string]any{"kind": "yrange", "start": base64.StdEncoding.EncodeToString(start),
		"end": base64.StdEncoding.EncodeToString(end), "lineage": "epoch-1", "quote": "Ship on Thursday"}
	newThread := map[string]any{"resource_kind": "doc", "resource_uuid": docID, "anchor": anchor, "body": "<p>Are we sure?</p>"}

	var made threadBusiness.View
	if code := call("ann", http.MethodPost, "/threads", newThread, &made); code != http.StatusOK {
		t.Fatalf("ann starting a thread: %d", code)
	}
	if made.Anchor.Quote != "Ship on Thursday" || !bytes.Equal(made.Anchor.Start, start) || !bytes.Equal(made.Anchor.End, end) || len(made.Comments) != 1 {
		t.Fatalf("the new thread: %+v", made)
	}
	if code := call("viewer", http.MethodPost, "/threads", newThread, nil); code != http.StatusForbidden {
		t.Errorf("a viewer starting a thread: %d", code)
	}
	if code := call("stranger", http.MethodGet, "/threads?resource_kind=doc&resource_uuid="+docID, nil, nil); code != http.StatusNotFound {
		t.Errorf("a stranger listing a private doc's threads: %d", code)
	}
	trashedThread := map[string]any{"resource_kind": "doc", "resource_uuid": trashed, "body": "<p>hi</p>"}
	if code := call("owner", http.MethodPost, "/threads", trashedThread, nil); code != http.StatusGone {
		t.Errorf("the owner commenting on a doc in the trash: %d", code)
	}

	// The comment is the doc's, in the graph, tagged with its thread.
	rows, _, err := threadComments.DocComments(ctx, docID)
	if err != nil {
		t.Fatal(err)
	}
	tagged := map[string]string{}
	for _, r := range rows {
		tagged[r.Uuid] = r.ThreadID
	}
	if tagged[made.Comments[0].ID] != made.ID || tagged[flat.Data.ID] != "" || len(rows) != 2 {
		t.Fatalf("the graph's comments: %v", tagged)
	}

	// Bob sees both: his flat comment as a page-level thread, and Ann's.
	got := list("bob", "")
	if len(got) != 2 || !got[0].Legacy || got[0].ID != flat.Data.ID || got[0].Anchor.Kind != threadBusiness.AnchorNone || got[1].ID != made.ID {
		t.Fatalf("bob's list: %+v", got)
	}
	if !got[1].Unread || got[1].CanResolve || !got[1].CanReply {
		t.Errorf("ann's thread to bob: %+v", got[1])
	}
	// Removing anyone's comment is the doc's owner's (or full access, or an
	// admin's), never a commenter's or a reader's: each view says which (B7).
	if made.CanModerate || got[1].CanModerate {
		t.Errorf("a commenter may remove anyone's comment: ann %v, bob %v", made.CanModerate, got[1].CanModerate)
	}
	for _, v := range list("owner", "") {
		if !v.CanModerate {
			t.Errorf("the owner may not remove comments on %s", v.ID)
		}
	}
	for _, v := range list("viewer", "") {
		if v.CanModerate {
			t.Errorf("a reader may remove comments on %s", v.ID)
		}
	}

	var reply threadBusiness.Comment
	// A file bob uploaded to the doc, named under another key, and ann's file
	// of a channel: a comment lists only its writer's own uploads to its
	// place, each with the key its upload stored (business/Attachment
	// OwnFiles), and shows its writer what it keeps, as it shows everyone.
	fileID, annsFile := uuid.NewString(), uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by)
		VALUES ($1, 'docComment/plan.pdf', 'doc', $2, $3), ($4, 'ann/salaries.xlsx', 'channel', $5, $6)`,
		fileID, docID, people["bob"], annsFile, uuid.NewString(), people["ann"]); err != nil {
		t.Fatal(err)
	}
	file := map[string]any{"uid": "0x1", "attachment_uuid": fileID, "attachment_file_name": "plan.pdf",
		"attachment_obj_key": "ann/salaries.xlsx", "attachment_type": "file"}
	anns := map[string]any{"attachment_uuid": annsFile, "attachment_file_name": "salaries.xlsx",
		"attachment_obj_key": "ann/salaries.xlsx", "attachment_type": "file"}
	if code := call("bob", http.MethodPost, "/threads/"+made.ID+"/replies",
		map[string]any{"body": "<p>Yes, Thursday.</p>", "attachments": []any{file, anns}}, &reply); code != http.StatusOK {
		t.Fatalf("bob replying: %d", code)
	}
	type shownFile struct {
		ID   string `json:"attachment_uuid"`
		Name string `json:"attachment_file_name"`
		Key  string `json:"attachment_obj_key"`
	}
	only := func(raw json.RawMessage) bool {
		var files []shownFile
		_ = json.Unmarshal(raw, &files)
		return slices.Equal(files, []shownFile{{ID: fileID, Name: "plan.pdf", Key: "docComment/plan.pdf"}})
	}
	if !only(reply.Attachments) {
		t.Errorf("bob's reply shows him %s; want only his own upload, with its key", reply.Attachments)
	}
	var mine threadBusiness.View
	call("ann", http.MethodGet, "/threads/"+made.ID, nil, &mine)
	if len(mine.Comments) != 2 || !mine.Unread {
		t.Fatalf("ann's thread after bob's reply: %+v", mine)
	}
	if !only(mine.Comments[1].Attachments) {
		t.Errorf("the reply's file reads back as %s", mine.Comments[1].Attachments)
	}
	if code := call("ann", http.MethodPost, "/threads/"+made.ID+"/read", nil, nil); code != http.StatusOK {
		t.Fatalf("mark read: %d", code)
	}
	call("ann", http.MethodGet, "/threads/"+made.ID, nil, &mine)
	if mine.Unread {
		t.Error("still unread after ann read it")
	}

	// Resolving: not bob (a commenter, not its author); the owner may.
	if code := call("bob", http.MethodPost, "/threads/"+made.ID+"/resolve", nil, nil); code != http.StatusForbidden {
		t.Errorf("bob resolving ann's thread: %d", code)
	}
	var resolved threadBusiness.Thread
	if code := call("owner", http.MethodPost, "/threads/"+made.ID+"/resolve", nil, &resolved); code != http.StatusOK || resolved.Status != threadBusiness.StatusResolved || resolved.ResolvedBy != people["owner"].String() {
		t.Fatalf("the owner resolving: %d %+v", code, resolved)
	}
	if open, done := list("bob", "open"), list("bob", "resolved"); len(open) != 1 || len(done) != 1 || done[0].ID != made.ID {
		t.Errorf("open %d, resolved %d", len(open), len(done))
	}
	if code := call("ann", http.MethodPost, "/threads/"+made.ID+"/reopen", nil, &resolved); code != http.StatusOK || resolved.Status != threadBusiness.StatusOpen {
		t.Errorf("ann reopening her thread: %d %+v", code, resolved)
	}

	// Ann replies to Bob's flat comment: it becomes a thread under its own id.
	if code := call("ann", http.MethodPost, "/threads/"+flat.Data.ID+"/replies", map[string]any{"body": "<p>Still true?</p>"}, nil); code != http.StatusOK {
		t.Fatalf("replying to a flat comment: %d", code)
	}
	var adopted threadBusiness.View
	call("bob", http.MethodGet, "/threads/"+flat.Data.ID, nil, &adopted)
	if adopted.Legacy || len(adopted.Comments) != 2 || adopted.CreatedBy != people["bob"].String() || !adopted.CanResolve {
		t.Fatalf("the adopted thread: %+v", adopted)
	}
	var stored int
	env.PG.QueryRow(`SELECT count(*) FROM threads WHERE id = $1 AND anchor_kind = 'none'`, flat.Data.ID).Scan(&stored)
	if stored != 1 {
		t.Errorf("the adopted thread is stored %d times", stored)
	}
	roles := map[string]string{}
	prs, _ := env.PG.Query(`SELECT user_uuid::text, role FROM thread_participants WHERE thread_id = $1`, made.ID)
	for prs.Next() {
		var u, r string
		prs.Scan(&u, &r)
		roles[u] = r
	}
	prs.Close()
	if roles[people["ann"].String()] != "author" || roles[people["bob"].String()] != "replier" {
		t.Errorf("participants: %v", roles)
	}

	// The old comment list still shows every comment, flat.
	var oldList struct {
		Data struct {
			Comments []json.RawMessage `json:"doc_comments"`
		} `json:"data"`
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("doc_uuid", docID)
	req = req.WithContext(context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, rctx), helpers.UserInfoContextKey, userInfo("bob")))
	docController.GetAllCommentList(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &oldList)
	if len(oldList.Data.Comments) != 4 {
		t.Errorf("the old list has %d comments, want 4", len(oldList.Data.Comments))
	}

	// "Reattach here": stored only when asked, by someone who may.
	moved := map[string]any{"anchor": map[string]any{"kind": "yrange", "start": base64.StdEncoding.EncodeToString([]byte{9}),
		"end": base64.StdEncoding.EncodeToString([]byte{10}), "lineage": "epoch-2", "quote": "Ship on Thursday"}}
	if code := call("bob", http.MethodPost, "/threads/"+made.ID+"/reanchor", moved, nil); code != http.StatusForbidden {
		t.Errorf("bob reattaching ann's thread: %d", code)
	}
	var re threadBusiness.Thread
	if code := call("ann", http.MethodPost, "/threads/"+made.ID+"/reanchor", moved, &re); code != http.StatusOK || re.Anchor.Lineage != "epoch-2" || !bytes.Equal(re.Anchor.Start, []byte{9}) {
		t.Fatalf("ann reattaching: %d %+v", code, re)
	}
	if code := call("ann", http.MethodGet, "/threads/"+uuid.NewString(), nil, nil); code != http.StatusNotFound {
		t.Errorf("no such thread: %d", code)
	}
}
