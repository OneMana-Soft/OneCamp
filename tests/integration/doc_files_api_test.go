//go:build integration
// +build integration

package integration_test

// Docs & Files' files over HTTP (docs F1), through the real handlers, on
// MinIO, Postgres 12 and Dgraph:
//   - POST /doc/files puts a file in a folder: a node with its upload as
//     version 1, answered as a tree row with its facts, sharing the folder's
//     sharing;
//   - POST /doc/files/{uuid}/versions adds a version for an editor, with a
//     note, and the title follows the new name; a viewer is refused;
//   - GET /doc/files/{uuid}/versions lists them newest first, each with who
//     added it, and each downloads through GET /doc/getFile/{file}/{id};
//   - a file put under a file is refused whole: no node, no rows, no bytes.
//
// Run: integ_rel.sh <wt> <log> '^TestAFileIsUploadedIntoTheTreeWithItsVersions$'

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	docController "github.com/akashc777/OneCamp/controllers/Doc"
	userController "github.com/akashc777/OneCamp/controllers/User"
	domainDoc "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAFileIsUploadedIntoTheTreeWithItsVersions(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	startMinio(t)

	people := map[string]uuid.UUID{"owner": uuid.New(), "viewer": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	uids := dg.Mutate(t, nodes)
	infoOf := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserPostgresInfo.EmailID = people[name].String() + "@example.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}

	now := time.Now()
	zero := time.Time{}
	yes, no := true, false
	folder := &dgraphStruct.DgraphDoc{DType: []string{"Doc"}, Uuid: uuid.NewString(), Title: "Contracts", NodeKind: domainDoc.NodeFolder, Position: "a0",
		IsPrivate: &yes, PublicComment: &no, CreatedBy: &dgraphStruct.DgraphUser{Uid: uids["owner"]}, Owner: &dgraphStruct.DgraphUser{Uid: uids["owner"]},
		EditingUser: []*dgraphStruct.DgraphUser{{Uid: uids["owner"]}}, ReadingUser: []*dgraphStruct.DgraphUser{{Uid: uids["viewer"]}},
		CreatedAt: &now, UpdatedAt: &now, DeletedAt: &zero}
	if _, err := domainDoc.CreateDocNodes(ctx, []domainDoc.NewDocNode{{Doc: folder, ParentRef: -1}}, nil, now); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Post("/doc/files", docController.AddDocFile)
	r.Post("/doc/files/{doc_uuid}/versions", docController.AddDocFileVersion)
	r.Get("/doc/files/{doc_uuid}/versions", docController.GetDocFileVersions)
	r.Get("/doc/getFile/{doc_uuid}/{obj_uuid}", userController.GetDocFile)
	send := func(who, method, path, name string, fields map[string]string) (int, map[string]any) {
		t.Helper()
		var body bytes.Buffer
		req := httptest.NewRequest(method, path, nil)
		if method == http.MethodPost {
			mw := multipart.NewWriter(&body)
			part, _ := mw.CreateFormFile("file", name)
			_, _ = part.Write([]byte("%PDF-1.4\n" + name + "\n%%EOF"))
			for k, v := range fields {
				_ = mw.WriteField(k, v)
			}
			_ = mw.Close()
			req = httptest.NewRequest(method, path, &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
		}
		req = req.WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, infoOf(who)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := send("owner", http.MethodPost, "/doc/files", "msa.pdf", map[string]string{"parent_uuid": folder.Uuid})
	row, _ := out["data"].(map[string]any)
	file, _ := row["file"].(map[string]any)
	if code != http.StatusOK || row["node_kind"] != "file" || row["title"] != "msa.pdf" || row["parent_uuid"] != folder.Uuid ||
		file["versions"] != float64(1) || file["name"] != "msa.pdf" || file["mime"] != "application/pdf" {
		t.Fatalf("a file into the folder: %d %v", code, out)
	}
	fileUUID, _ := row["uuid"].(string)
	firstAtt, _ := file["attachment_id"].(string)

	code, out = send("owner", http.MethodPost, "/doc/files/"+fileUUID+"/versions", "msa-signed.pdf", map[string]string{"note": "Signed by both"})
	version, _ := out["data"].(map[string]any)
	if code != http.StatusOK || version["n"] != float64(2) || version["note"] != "Signed by both" || version["name"] != "msa-signed.pdf" {
		t.Fatalf("a new version: %d %v", code, out)
	}
	stored, err := domainDoc.GetCollabDocByUUID(ctx, fileUUID)
	if err != nil || stored.Title != "msa-signed.pdf" {
		t.Errorf("the title didn't follow the new version: %+v %v", stored, err)
	}
	if code, out := send("viewer", http.MethodPost, "/doc/files/"+fileUUID+"/versions", "mine.pdf", nil); code != http.StatusForbidden {
		t.Errorf("a viewer's version: %d %v", code, out)
	}

	code, out = send("viewer", http.MethodGet, "/doc/files/"+fileUUID+"/versions", "", nil)
	data, _ := out["data"].(map[string]any)
	versions, _ := data["versions"].([]any)
	if code != http.StatusOK || len(versions) != 2 {
		t.Fatalf("the versions, as a viewer: %d %v", code, out)
	}
	newest, _ := versions[0].(map[string]any)
	by, _ := newest["created_by"].(map[string]any)
	if newest["n"] != float64(2) || by["user_uuid"] != people["owner"].String() || by["user_name"] != "owner" {
		t.Errorf("the newest version: %v", newest)
	}
	for _, att := range []string{firstAtt, newest["attachment_id"].(string)} {
		if code, out := send("viewer", http.MethodGet, "/doc/getFile/"+fileUUID+"/"+att, "", nil); code != http.StatusOK || out["url"] == nil {
			t.Errorf("downloading version %s as a viewer: %d %v", att, code, out)
		}
	}

	var attachments, versionRows int
	if err := env.PG.QueryRow(`SELECT (SELECT count(*) FROM attachments), (SELECT count(*) FROM doc_file_versions)`).Scan(&attachments, &versionRows); err != nil {
		t.Fatal(err)
	}
	if code, out := send("owner", http.MethodPost, "/doc/files", "inside.pdf", map[string]string{"parent_uuid": fileUUID}); code != http.StatusConflict || out["code"] != "cannot_hold_items" {
		t.Errorf("a file under a file: %d %v", code, out)
	}
	var afterAttachments, afterVersions int
	if err := env.PG.QueryRow(`SELECT (SELECT count(*) FROM attachments), (SELECT count(*) FROM doc_file_versions)`).Scan(&afterAttachments, &afterVersions); err != nil {
		t.Fatal(err)
	}
	if afterAttachments != attachments || afterVersions != versionRows {
		t.Errorf("a refused file left rows: attachments %d -> %d, versions %d -> %d", attachments, afterAttachments, versionRows, afterVersions)
	}
}
