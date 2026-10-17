//go:build integration
// +build integration

package integration_test

// "That message was deleted" and "This doc doesn't exist, or you can't open
// it." are answers about the message or the doc. A database that doesn't
// answer isn't one: it's a 5xx, which the web app reads as "try again", and
// the person isn't told their message or doc is gone. A doc's two reads here,
// an edit's and the
// collaboration service's, answer by the one rule for a read of one thing
// (helpers.WriteDocReadFailure: 404 in its one sentence, or 503 for a read
// that failed), as does a decision the tree couldn't make; and a doc the
// editor can't open answers exactly as one that doesn't exist.
//
// Run: go test -tags=integration ./tests/integration/ -run TestNotFoundOrFault -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestNotFoundOrFault(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Kim's plans are a private doc Sam can't open.
	sam, kim, general, deleted, plans := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'sam', 'Sam', NOW(), NOW())`, sam, sam.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'general', false)`, general); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by, deleted_at) VALUES ($1, $2, $3, NOW())`, deleted, general, sam); err != nil {
		t.Fatal(err)
	}
	live := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:kim", "dgraph.type": "User", "user_uuid": kim.String(), "user_name": "kim"},
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general",
			"ch_private": false, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:sam"}},
			"ch_posts": []map[string]any{{"uid": "_:post", "dgraph.type": "Post", "post_uuid": deleted.String(),
				"post_text": "<p>gone</p>", "post_by": map[string]any{"uid": "_:sam"},
				"post_channel": map[string]any{"uid": "_:general"}, "post_deleted_at": live}}},
		{"uid": "_:plans", "dgraph.type": "Doc", "doc_uuid": plans.String(), "doc_title": "Kim's plans",
			"doc_private": true, "doc_public_comment": false, "doc_public_edit": false, "doc_deleted_at": live,
			"doc_created_at": "2026-10-14T05:00:00Z", "doc_updated_at": "2026-10-14T05:00:00Z",
			"doc_body": "<p>Q4</p>", "doc_created_by": map[string]any{"uid": "_:kim"}},
	})

	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, sam)
	if err != nil || pg == nil {
		t.Fatalf("load sam: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
	if err != nil || gr == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	asSam := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})

	call := func(handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rctx, cancel := context.WithTimeout(asSam, 20*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(rctx))
		return rec.Code, rec.Body.String()
	}
	title := "plans"
	editPost := func() (int, string) {
		return call(postController.UpdatePost, postAdapter.InputCreateOrUpdatePostInfo{Uuid: deleted.String(), HTMLText: "<p>back</p>"})
	}
	editDoc := func(id string) (int, string) {
		return call(docController.UpdateDocBodyLive, docAdapter.InputUpdateDoc{DocId: id, Title: &title})
	}
	// The collaboration service's read of a doc it opens: no person asks it.
	collabRead := func(id string) (int, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add("doc_uuid", id)
		rctx, cancel := context.WithTimeout(context.WithValue(ctx, chi.RouteCtxKey, rc), 20*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		docController.GetDocForCollab(rec, httptest.NewRequest(http.MethodGet, "/docColab/getDoc/"+id, nil).WithContext(rctx))
		return rec.Code, rec.Body.String()
	}

	// Answers: the message was deleted; there's no such doc, and a doc Sam
	// can't open is no more than that to him, word for word.
	if code, body := editPost(); code != http.StatusNotFound {
		t.Errorf("editing a deleted message: %d %s, want 404", code, body)
	}
	missingCode, missingBody := editDoc(uuid.NewString())
	if missingCode != http.StatusNotFound {
		t.Errorf("editing a doc that doesn't exist: %d %s, want 404", missingCode, missingBody)
	}
	if code, body := editDoc(plans.String()); code != missingCode || body != missingBody {
		t.Errorf("editing a doc Sam can't open: %d %s, where a doc that doesn't exist answers %d %s", code, body, missingCode, missingBody)
	}
	if code, body := collabRead(plans.String()); code != http.StatusOK {
		t.Errorf("the collaboration service reading a doc: %d %s, want 200", code, body)
	}
	if code, body := collabRead(uuid.NewString()); code != http.StatusNotFound {
		t.Errorf("the collaboration service reading a doc that doesn't exist: %d %s, want 404", code, body)
	}

	// Postgres stops answering: the message isn't "deleted".
	if err := postgresInit.DBConn.SqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if code, body := editPost(); code != http.StatusInternalServerError {
		t.Errorf("editing a message while Postgres doesn't answer: %d %s, want 500", code, body)
	}

	// Dgraph stops answering: no doc is missing, and either read may be tried
	// again.
	dg.Close()
	if code, body := editDoc(plans.String()); code != http.StatusServiceUnavailable {
		t.Errorf("editing a doc while Dgraph doesn't answer: %d %s, want 503", code, body)
	}
	if code, body := collabRead(plans.String()); code != http.StatusServiceUnavailable {
		t.Errorf("the collaboration service reading a doc while Dgraph doesn't answer: %d %s, want 503", code, body)
	}
	// The tree decides before it does anything, and a decision that couldn't
	// be made is the same failed read, never "something went wrong" (500).
	if code, body := call(docController.DeleteDocTree, map[string]any{"doc_uuid": plans.String()}); code != http.StatusServiceUnavailable {
		t.Errorf("deleting a doc while Dgraph doesn't answer: %d %s, want 503", code, body)
	}
}
