//go:build integration
// +build integration

package integration_test

// "That message was deleted" and "There's no such doc" are answers about the
// message or the doc. A database that doesn't answer isn't one: it's a 5xx,
// which the web app reads as "try again", and the person isn't told their
// message or doc is gone.
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

	sam, general, deleted := uuid.New(), uuid.New(), uuid.New()
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
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general",
			"ch_private": false, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:sam"}},
			"ch_posts": []map[string]any{{"uid": "_:post", "dgraph.type": "Post", "post_uuid": deleted.String(),
				"post_text": "<p>gone</p>", "post_by": map[string]any{"uid": "_:sam"},
				"post_channel": map[string]any{"uid": "_:general"}, "post_deleted_at": live}}},
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
	editDoc := func() (int, string) {
		return call(docController.UpdateDocBody, docAdapter.InputUpdateDoc{DocId: uuid.NewString(), Title: &title})
	}

	// Answers: the message was deleted; there's no such doc.
	if code, body := editPost(); code != http.StatusNotFound {
		t.Errorf("editing a deleted message: %d %s, want 404", code, body)
	}
	if code, body := editDoc(); code != http.StatusNotFound {
		t.Errorf("editing a doc that doesn't exist: %d %s, want 404", code, body)
	}

	// Postgres stops answering: the message isn't "deleted".
	if err := postgresInit.DBConn.SqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if code, body := editPost(); code != http.StatusInternalServerError {
		t.Errorf("editing a message while Postgres doesn't answer: %d %s, want 500", code, body)
	}

	// Dgraph stops answering: the doc isn't missing.
	dg.Close()
	if code, body := editDoc(); code != http.StatusInternalServerError {
		t.Errorf("editing a doc while Dgraph doesn't answer: %d %s, want 500", code, body)
	}
}
