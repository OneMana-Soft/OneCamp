//go:build integration
// +build integration

package integration_test

// A file uploaded for a doc (POST /uploadFile, src_key "doc") is the doc's:
// it is served to everyone who may read the doc. So uploading one takes the
// access a comment does (a doc's comments take files this way too; putting
// one in the body is an edit, which saving the body asks for). Someone who
// may only view the doc is refused, and someone it isn't shared with gets the
// 404 a doc that isn't there gets, learning nothing. Nothing is stored for
// either.
//
// Before this, anyone signed in could upload a file as any doc's, by naming
// it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestOnlyThoseWhoMayAddToADocUploadItsFiles -v

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestOnlyThoseWhoMayAddToADocUploadItsFiles(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	startMinio(t)

	people := map[string]uuid.UUID{"owner": uuid.New(), "editor": uuid.New(), "commenter": uuid.New(), "viewer": uuid.New(), "stranger": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	doc := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Salary bands",
		"doc_body": "<p>Only the founders read this.</p>", "doc_private": true, "doc_public_comment": false,
		"doc_created_at": "2026-10-01T00:00:00Z", "doc_deleted_at": "0001-01-01T00:00:00Z",
		"doc_created_by":       map[string]any{"uid": "_:owner"},
		"doc_editing_users":    []map[string]any{{"uid": "_:owner"}, {"uid": "_:editor"}},
		"doc_commenting_users": []map[string]any{{"uid": "_:commenter"}},
		"doc_reading_users":    []map[string]any{{"uid": "_:viewer"}}})
	dg.Mutate(t, nodes)

	// upload posts a file for the doc as /uploadFile's sign-in
	// (VerifyAuthOnlyPostgres) leaves the request: the Postgres account only.
	upload := func(who, docUUID string) (int, map[string]any) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, _ := mw.CreateFormFile("file", "bands.txt")
		_, _ = part.Write([]byte("band A, band B"))
		_ = mw.WriteField("jsonData", `{"src_key":"doc","src_value":"`+docUUID+`"}`)
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/uploadFile", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = people[who].String() + "@example.test"
		req = req.WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, u))
		rec := httptest.NewRecorder()
		userController.UploadFile(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	stored := func(who string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM attachments WHERE src_key = 'doc' AND src_value = $1 AND created_by = $2`, doc, people[who]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("someone who may only view the doc is refused", func(t *testing.T) {
		if code, body := upload("viewer", doc); code != http.StatusForbidden {
			t.Fatalf("a viewer: %d %v", code, body)
		}
		if n := stored("viewer"); n != 0 {
			t.Fatalf("a viewer's file was stored as the doc's (%d)", n)
		}
	})

	t.Run("someone it isn't shared with learns nothing", func(t *testing.T) {
		code, body := upload("stranger", doc)
		missing, missingBody := upload("stranger", uuid.NewString())
		if code != http.StatusNotFound || missing != http.StatusNotFound || body["msg"] != missingBody["msg"] {
			t.Fatalf("a stranger: %d %v; a doc that isn't there: %d %v", code, body, missing, missingBody)
		}
		if n := stored("stranger"); n != 0 {
			t.Fatalf("a stranger's file was stored as the doc's (%d)", n)
		}
	})

	for _, who := range []string{"editor", "commenter", "owner"} {
		t.Run(who+" uploads a file for it", func(t *testing.T) {
			code, body := upload(who, doc)
			if code != http.StatusOK || body["object_uuid"] == nil {
				t.Fatalf("%s: %d %v", who, code, body)
			}
			if n := stored(who); n != 1 {
				t.Fatalf("%s's file is stored %d times as the doc's", who, n)
			}
		})
	}

	// With file storage down, someone who may add the file is told storage
	// isn't answering and to try again in a moment: a 503 in words the web
	// shows as they are. It used to be a 400 the web read as "Something in the
	// request wasn't accepted", which sent people looking at their file.
	t.Run("with storage down the uploader is told so", func(t *testing.T) {
		down, err := minio.New("127.0.0.1:1", &minio.Options{Creds: credentials.NewStaticV4("onecamp", "onecamp-secret", "")})
		if err != nil {
			t.Fatal(err)
		}
		up := minioInit.MinioClient
		minioInit.MinioClient = down
		defer func() { minioInit.MinioClient = up }()

		code, body := upload("editor", doc)
		if code != http.StatusServiceUnavailable || body["msg"] != "File storage isn't answering right now. Try again in a moment." {
			t.Fatalf("storage down: %d %v", code, body)
		}
		if n := stored("editor"); n != 1 {
			t.Fatalf("a file that was never saved is listed as the doc's (%d)", n)
		}
	})
}
