//go:build integration
// +build integration

package integration_test

// Archiving docs by the workspace's retention, and bringing them back, end to
// end: a real archive run (Postgres jobs, the graph), the restore list, undo,
// an admin restoring a doc its owner deleted, and search following each move.
//
// Every doc archive and restore failed: the graph was sent a query that is
// not valid DQL. Restore cleared a search field no index has (doc_id), docs
// were picked by when they were made rather than last changed, a run recorded
// nothing to undo, and the restore list called every doc "Document · <time>"
// and mixed docs their owners deleted in with archived ones. Then a comment
// its author deleted came back to search, global and AI, when its doc was
// restored: the run's cascade wrote its own marker over the comment's entries,
// and the delete itself marked the AI entry as the cascade does. And a doc
// brought back was archived again by the next hourly run, since a restore
// left its last edit a year old. The restore list showed every doc's title
// to any admin, private ones included, and restoring a doc its owner deleted
// brought its guest links back to life.
//
// Run: go test -tags=integration ./tests/integration/ -run TestArchivingAndRestoringADoc -v

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	archiveBusiness "github.com/akashc777/OneCamp/business/Archive"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestArchivingAndRestoringADoc(t *testing.T) {
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
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, 3); err != nil {
		t.Fatalf("create the AI index: %v", err)
	}
	client := opensearchInit.OpenSearchClient

	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'dana', 'Dana Whitfield', NOW(), NOW())`, owner, owner.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, -n).Format(time.RFC3339) }
	live := "0001-01-01T00:00:00Z"
	ids := map[string]string{}
	doc := func(name string, created, updated, deleted string) map[string]any {
		ids[name] = uuid.NewString()
		n := map[string]any{"uid": "_:" + strings.ReplaceAll(name, " ", "_"), "dgraph.type": "Doc", "doc_uuid": ids[name], "doc_title": name,
			"doc_private": false, "doc_public_comment": true, "doc_created_at": created, "doc_deleted_at": deleted,
			"doc_created_by": map[string]any{"uid": "_:owner"}, "doc_editing_users": []map[string]any{{"uid": "_:owner"}}}
		if updated != "" {
			n["doc_updated_at"] = updated
		}
		return n
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "Dana Whitfield"},
		doc("Old plan", day(400), day(400), live),
		doc("Old notes never edited", day(400), "", live),
		doc("Old but edited yesterday", day(400), day(1), live),
		doc("Recent draft", day(5), day(5), live),
		doc("Deleted by its owner", day(400), day(400), day(2)),
	})
	// Two are private, and shared with nobody but Dana, who made them.
	dg.Mutate(t, []map[string]any{
		{"uid": uids["Old_notes_never_edited"], "doc_private": true},
		{"uid": uids["Deleted_by_its_owner"], "doc_private": true},
	})
	if _, err := docBusiness.RepairDocSearch(ctx); err != nil {
		t.Fatalf("index the docs: %v", err)
	}
	// A guest link to an archived doc and one to the doc Dana deleted.
	links := map[string]uuid.UUID{}
	for _, name := range []string{"Old plan", "Deleted by its owner"} {
		token := sha256.Sum256([]byte(uuid.NewString()))
		expires := time.Now().Add(30 * 24 * time.Hour)
		id, err := guestModel.CreateGrant(ctx, token[:], guestModel.ResourceDoc, ids[name], guestModel.CapabilityView, owner, &expires)
		if err != nil {
			t.Fatalf("a guest link to %s: %v", name, err)
		}
		links[name] = id
	}
	linkOn := func(name string) bool {
		t.Helper()
		g, err := guestModel.GetByID(ctx, links[name])
		if err != nil || g == nil {
			t.Fatalf("read the guest link to %s: %v", name, err)
		}
		return g.RevokedAt == nil
	}
	// Dana's phone, and what is pushed to anyone.
	if _, err := env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'danas-phone')`, owner); err != nil {
		t.Fatal(err)
	}
	var pushMu sync.Mutex
	var pushes []map[string]string
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, _ []string) {
		pushMu.Lock()
		pushes = append(pushes, data)
		pushMu.Unlock()
	})()
	// Sam is an admin none of the docs is shared with.
	var sam userModels.UserInfo
	sam.UserPostgresInfo.Id = uuid.New()
	sam.UserDgraphInfo = dgraphStruct.DgraphUser{Uuid: sam.UserPostgresInfo.Id.String(), UserName: "Sam Rivera"}
	as := func(u userModels.UserInfo) context.Context {
		return context.WithValue(ctx, helpers.UserInfoContextKey, u)
	}
	var who userModels.UserInfo
	who.UserPostgresInfo.Id = owner
	who.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["owner"], Uuid: owner.String(), UserName: "Dana Whitfield"}
	plan, err := docBusiness.GetBasicDgraphDocByUUID(ctx, ids["Old plan"], uids["owner"])
	if err != nil {
		t.Fatal(err)
	}
	comment, err := docBusiness.CreateDocComment(ctx, plan, &who, &docAdapter.CreateOrUpdateDocCommentInput{DocUuid: ids["Old plan"], CommentBody: "<p>signed off</p>"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// One its author deletes before any run, which must stay deleted.
	gone, err := docBusiness.CreateDocComment(ctx, plan, &who, &docAdapter.CreateOrUpdateDocCommentInput{DocUuid: ids["Old plan"], CommentBody: "<p>wrong numbers, ignore</p>"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	entry := func(index, id string) map[string]any {
		t.Helper()
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: index, DocumentID: id})
		if err != nil {
			return nil
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Source map[string]any `json:"_source"`
		}
		_ = json.Unmarshal(raw, &got)
		return got.Source
	}
	deletedInSearch := func(index, id string) bool { e := entry(index, id); return e != nil && e["deleted_date"] != nil }
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting until %s", what)
	}
	// searchable has a search see what the comments index holds. A run
	// marks a doc's comments, and an undo unmarks them, through a search over
	// them, which sees only what the index has refreshed (within a second),
	// while the entry reads here see each write at once. So a comment written,
	// or marked, the moment before the next run was often missed by it. In
	// use, a retention run comes long after a comment, and an undo after the
	// run.
	searchable := func() {
		t.Helper()
		if _, err := client.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{"comments", "ai_embeddings"}}); err != nil {
			t.Fatalf("refresh the comments and AI indices: %v", err)
		}
	}
	eventually("the comments are in search", func() bool { return entry("comments", comment.Uuid) != nil && entry("comments", gone.Uuid) != nil })
	// The doc's and its comments' AI entries, as the AI index writes them
	// (there is no model here to write them).
	for id, body := range map[string]map[string]any{
		"doc:" + ids["Old plan"]:  {"content_type": "doc", "content_uuid": ids["Old plan"], "content_text": "Old plan"},
		"comment:" + comment.Uuid: {"content_type": "comment", "content_uuid": comment.Uuid, "doc_uuid": ids["Old plan"], "content_text": "signed off"},
		"comment:" + gone.Uuid:    {"content_type": "comment", "content_uuid": gone.Uuid, "doc_uuid": ids["Old plan"], "content_text": "wrong numbers, ignore"},
	} {
		raw, _ := json.Marshal(body)
		if _, err := client.Index(ctx, opensearchapi.IndexReq{Index: "ai_embeddings", DocumentID: id, Body: strings.NewReader(string(raw)),
			Params: opensearchapi.IndexParams{Refresh: "true"}}); err != nil {
			t.Fatalf("write the AI entry %s: %v", id, err)
		}
	}
	searchable()
	goneComment, err := commentBusiness.GetDgraphDocCommentInfoByUUID(ctx, gone.Uuid, uids["owner"])
	if err != nil || goneComment == nil {
		t.Fatalf("read the comment to delete: %v", err)
	}
	if err := docBusiness.DeleteDocComment(ctx, uuid.MustParse(gone.Uuid), ids["Old plan"], goneComment); err != nil {
		t.Fatalf("delete the comment: %v", err)
	}
	eventually("search shows the deleted comment as gone", func() bool {
		return deletedInSearch("comments", gone.Uuid) && deletedInSearch("ai_embeddings", "comment:"+gone.Uuid)
	})
	searchable()
	// staysDeleted says what search shows of the deleted comment, once what
	// was restored is back (a restore marks everything in one go).
	staysDeleted := func(when string) {
		t.Helper()
		for _, index := range []string{"comments", "ai_embeddings"} {
			id := gone.Uuid
			if index == "ai_embeddings" {
				id = "comment:" + gone.Uuid
			}
			if !deletedInSearch(index, id) {
				t.Errorf("%s, the comment its author deleted is back in %s: %v", when, index, entry(index, id))
			}
		}
	}
	// searchShows is eventually for what search holds, then searchable, and
	// says on a timeout what search held for the doc and its comment.
	searchShows := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				searchable()
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting until %s; the doc's entry: %v; its comment's: %v", what, entry("docs", ids["Old plan"]), entry("comments", comment.Uuid))
	}

	type state struct {
		deleted, archived bool
	}
	stateOf := func(name string) state {
		t.Helper()
		raw := dg.Query(t, fmt.Sprintf(`{ d(func: eq(doc_uuid, %q)) { doc_deleted_at doc_archived_at } }`, ids[name]))
		var got struct {
			D []dgraphStruct.DgraphDoc `json:"d"`
		}
		if err := json.Unmarshal(raw, &got); err != nil || len(got.D) != 1 {
			t.Fatalf("read %s: %v %s", name, err, raw)
		}
		return state{deleted: docBusiness.IsDeleted(&got.D[0]), archived: got.D[0].ArchivedAt != nil && got.D[0].ArchivedAt.After(time.Unix(0, 0))}
	}
	nodes := func() int {
		t.Helper()
		var got struct {
			N []struct {
				Count int `json:"count"`
			} `json:"n"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ n(func: has(doc_deleted_at)) { count(uid) } }`), &got)
		return got.N[0].Count
	}
	runArchive := func(want int) string {
		t.Helper()
		job, err := archiveBusiness.RunArchiveJob(ctx, "docs", nil)
		if err != nil {
			t.Fatalf("start the archive run: %v", err)
		}
		var status string
		var archived int
		eventually("the archive run finishes", func() bool {
			_ = env.PG.QueryRow(`SELECT status, items_archived FROM archive_jobs WHERE id = $1`, job).Scan(&status, &archived)
			return status != "running"
		})
		if status != "completed" {
			var msg *string
			_ = env.PG.QueryRow(`SELECT error_message FROM archive_jobs WHERE id = $1`, job).Scan(&msg)
			t.Fatalf("the archive run %s: %v", status, msg)
		}
		if archived != want {
			t.Fatalf("the archive run archived %d docs, want %d", archived, want)
		}
		return job
	}
	before := nodes()

	// A run archives the docs nobody has changed in a year: by last change, so
	// one made long ago and edited yesterday stays.
	job := runArchive(2)
	for name, want := range map[string]state{
		"Old plan": {true, true}, "Old notes never edited": {true, true},
		"Old but edited yesterday": {false, false}, "Recent draft": {false, false}, "Deleted by its owner": {true, false},
	} {
		if got := stateOf(name); got != want {
			t.Errorf("%s after the archive run: %+v, want %+v", name, got, want)
		}
	}
	searchShows("search shows the archived doc and its comment as gone", func() bool {
		return deletedInSearch("docs", ids["Old plan"]) && deletedInSearch("comments", comment.Uuid) &&
			deletedInSearch("ai_embeddings", "doc:"+ids["Old plan"]) && deletedInSearch("ai_embeddings", "comment:"+comment.Uuid)
	})
	staysDeleted("after the archive run")
	if deletedInSearch("docs", ids["Old but edited yesterday"]) {
		t.Error("search shows a doc in use as archived")
	}

	// The restore list: titles, and whose choice a deletion was; a private doc
	// the admin asking may not read is "Private doc" and whose it is.
	for admin, wants := range map[string]map[string]string{
		"Dana": {
			"Old plan": "Old plan", "Old notes never edited": "Old notes never edited",
			"Deleted by its owner": "Deleted by its owner · deleted by Dana Whitfield",
		},
		"Sam": {
			"Old plan": "Old plan", "Old notes never edited": "Private doc · owned by Dana Whitfield",
			"Deleted by its owner": "Private doc · deleted by Dana Whitfield",
		},
	} {
		asker := who
		if admin == "Sam" {
			asker = sam
		}
		items, total, err := archiveBusiness.GetRecentlyArchivedItems(as(asker), "docs", 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]string{}
		for _, it := range items {
			names[it.Id] = it.Name
		}
		for name, want := range wants {
			if names[ids[name]] != want {
				t.Errorf("the restore list names %s %q to %s, want %q", name, names[ids[name]], admin, want)
			}
		}
		if total != 3 || len(items) != 3 {
			t.Errorf("the restore list holds %d of %d for %s, want the 3 deleted docs", len(items), total, admin)
		}
	}

	// Undo brings back what the run archived, and nothing else.
	if n, err := archiveBusiness.UndoArchiveJob(ctx, uuid.MustParse(job)); err != nil || n != 2 {
		t.Fatalf("undoing the run: %d, %v", n, err)
	}
	for _, name := range []string{"Old plan", "Old notes never edited"} {
		if got := stateOf(name); got != (state{}) {
			t.Errorf("%s after undo: %+v, want live and not marked archived", name, got)
		}
	}
	if !stateOf("Deleted by its owner").deleted {
		t.Error("undoing an archive run brought back a doc its owner deleted")
	}
	searchShows("search shows the doc and its comment again", func() bool {
		return !deletedInSearch("docs", ids["Old plan"]) && !deletedInSearch("comments", comment.Uuid) && entry("docs", ids["Old plan"]) != nil &&
			!deletedInSearch("ai_embeddings", "doc:"+ids["Old plan"]) && !deletedInSearch("ai_embeddings", "comment:"+comment.Uuid)
	})
	staysDeleted("after undo")
	if !linkOn("Old plan") {
		t.Error("undoing a run turned off the guest link of a doc it brought back")
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("doc_uuid", ids["Old plan"])
	rec := httptest.NewRecorder()
	docController.GetDocPage(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(
		context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, rctx), helpers.UserInfoContextKey, who)))
	if rec.Code != http.StatusOK {
		t.Errorf("opening the restored doc: %d", rec.Code)
	}

	// The run after undo leaves the docs it brought back: a restored doc waits
	// out the whole retention period from its restore, however old its last
	// edit. The next hourly run archived them again.
	runArchive(0)
	for _, name := range []string{"Old plan", "Old notes never edited"} {
		if got := stateOf(name); got != (state{}) {
			t.Errorf("%s after the run that followed undo: %+v, want live", name, got)
		}
	}
	// Once that period has gone by with nobody touching them, they are due.
	dg.Mutate(t, []map[string]any{
		{"uid": uids["Old_plan"], "doc_restored_at": day(366)},
		{"uid": uids["Old_notes_never_edited"], "doc_restored_at": day(366)},
	})

	// An admin restoring from the list brings back an archived doc, and one
	// its owner deleted (there is no Trash in the app yet). In use the restore
	// comes long after the run's search writes land; here it waits for them.
	runArchive(2)
	searchShows("search shows the doc's comment as gone again", func() bool {
		return deletedInSearch("comments", comment.Uuid) && deletedInSearch("ai_embeddings", "comment:"+comment.Uuid)
	})
	n, err := archiveBusiness.RestoreItems(as(sam), "docs", []uuid.UUID{uuid.MustParse(ids["Old plan"]), uuid.MustParse(ids["Deleted by its owner"]),
		uuid.MustParse(ids["Recent draft"]), uuid.New()})
	if err != nil || n != 2 {
		t.Fatalf("restoring from the list: %d, %v; want the 2 deleted docs (a live one and a doc that doesn't exist count for nothing)", n, err)
	}
	for _, name := range []string{"Old plan", "Deleted by its owner", "Recent draft"} {
		if got := stateOf(name); got != (state{}) {
			t.Errorf("%s after restoring: %+v, want live", name, got)
		}
	}
	eventually("search shows the owner's restored doc", func() bool {
		return entry("docs", ids["Deleted by its owner"]) != nil && !deletedInSearch("docs", ids["Deleted by its owner"])
	})
	searchShows("search shows the restored doc's comment again", func() bool {
		return !deletedInSearch("comments", comment.Uuid) && !deletedInSearch("ai_embeddings", "comment:"+comment.Uuid)
	})
	staysDeleted("after an admin restored the doc")
	// The doc Dana deleted comes back with its guest link off, and Dana is
	// told; the archived one keeps its link.
	if linkOn("Deleted by its owner") {
		t.Error("restoring a doc its owner deleted left its guest link on")
	}
	if !linkOn("Old plan") {
		t.Error("restoring an archived doc turned off its guest link")
	}
	pushMu.Lock()
	var told []map[string]string
	for _, p := range pushes {
		if p[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] == ids["Deleted by its owner"] || p[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] == ids["Old plan"] {
			told = append(told, p)
		}
	}
	pushMu.Unlock()
	if want := "Sam Rivera restored “Deleted by its owner”, which you deleted. Its guest link was turned off; you can turn guest access back on by sharing it again."; len(told) != 1 ||
		told[0][firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != ids["Deleted by its owner"] || told[0][firebaseInit.FIREBASE_PUSH_DATA_BODY] != want {
		t.Errorf("pushed about the restored docs: %v; want Dana told once, about the doc she deleted: %q", told, want)
	}
	// And the run after leaves what the admin brought back.
	runArchive(0)
	for _, name := range []string{"Old plan", "Deleted by its owner"} {
		if got := stateOf(name); got != (state{}) {
			t.Errorf("%s after the run that followed the admin's restore: %+v, want live", name, got)
		}
	}
	if after := nodes(); after != before {
		t.Errorf("archiving and restoring made %d new nodes; a uuid with no doc must make none", after-before)
	}
}

// A run picks the docs nobody has touched, then archives them. A doc edited
// in between was archived in use: the archive checked only that it was live.
// And a save that had read a doc live still landed in it once archived, since
// the save and the archive wrote nothing in common, which is all Dgraph
// checks. Against a real Dgraph with the production schema.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheArchiveLeavesADocInUse -v
func TestTheArchiveLeavesADocInUse(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, -n).Format(time.RFC3339) }
	cutoff := time.Now().AddDate(-1, 0, 0)
	ids := map[string]string{"edited": uuid.NewString(), "saved": uuid.NewString()}
	var nodes []map[string]any
	for name, id := range ids {
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "Doc", "doc_uuid": id, "doc_title": name,
			"doc_body": "<p>as it was</p>", "doc_created_at": day(400), "doc_updated_at": day(400), "doc_deleted_at": "0001-01-01T00:00:00Z"})
	}
	uids := dg.Mutate(t, nodes)
	due, err := docDomain.GetDocsInactiveSince(ctx, cutoff, 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("the run picks %v (%v), want both docs", due, err)
	}

	// Someone edits one after the run picked it.
	dg.Mutate(t, []map[string]any{{"uid": uids["edited"], "doc_updated_at": time.Now().UTC().Format(time.RFC3339)}})
	// Someone saves the other as the archive runs: the save has read the doc
	// live, writes what a save writes, and commits after the archive.
	save := dgraphInit.DgraphClient.NewTxn()
	defer save.Discard(ctx)
	if _, err := save.Query(ctx, fmt.Sprintf(`{ d(func: eq(doc_uuid, %q)) @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) { uid } }`, ids["saved"])); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"uid": uids["saved"], "doc_uuid": ids["saved"], "doc_body": "<p>saved into the archive</p>", "doc_updated_at": time.Now().UTC()})
	if _, err := save.Mutate(ctx, &api.Mutation{SetJson: body}); err != nil {
		t.Fatal(err)
	}

	archived, err := docDomain.BulkArchiveDocs(ctx, due, cutoff)
	if err != nil {
		t.Fatalf("the archive: %v", err)
	}
	if len(archived) != 1 || archived[0] != ids["saved"] {
		t.Errorf("the archive took %v; want only the doc nobody edited after the run picked it (%s)", archived, ids["saved"])
	}
	if err := save.Commit(ctx); !errors.Is(err, dgo.ErrAborted) {
		t.Errorf("a save that read the doc live landed after it was archived (its commit: %v); want it refused, to run again and find the doc archived", err)
	}

	var got struct {
		D []dgraphStruct.DgraphDoc `json:"d"`
	}
	raw := dg.Query(t, fmt.Sprintf(`{ d(func: eq(doc_uuid, [%q, %q])) { doc_uuid doc_body doc_deleted_at doc_archived_at } }`, ids["edited"], ids["saved"]))
	if err := json.Unmarshal(raw, &got); err != nil || len(got.D) != 2 {
		t.Fatalf("read the docs back: %v %s", err, raw)
	}
	for _, d := range got.D {
		archivedNow := d.ArchivedAt != nil && d.ArchivedAt.After(time.Unix(0, 0))
		switch d.Uuid {
		case ids["edited"]:
			if docBusiness.IsDeleted(&d) || archivedNow {
				t.Errorf("the doc edited after the run picked it was archived in use")
			}
		case ids["saved"]:
			if !docBusiness.IsDeleted(&d) || !archivedNow || d.Body != "<p>as it was</p>" {
				t.Errorf("the doc archived as a save was in flight: archived %v, body %q; want archived, with the body it had", archivedNow, d.Body)
			}
		}
	}
}
