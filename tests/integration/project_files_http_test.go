//go:build integration
// +build integration

package integration_test

// A project's Files tab, through the handlers the web calls, in its order
// (components/project/projectFiles.tsx): POST /uploadFile with the project as
// the place, POST /project/addAttachment with what the upload answered, GET
// /project/attachments/{project}, and POST /project/removeAttachment with the
// file's id. The owner found files could neither be added nor removed on
// beta (23 Oct 2026), and nothing tested this path end to end.
//
// Run: integ_rel.sh <wt> <log> '^TestAProjectsFilesAreAddedListedAndRemovedThroughTheHandlers$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAProjectsFilesAreAddedListedAndRemovedThroughTheHandlers(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	startMinio(t)

	const live = "0001-01-01T00:00:00Z"
	ada, bob, project := uuid.New(), uuid.New(), uuid.New()
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{ada, "ada"}, {bob, "bob"}} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, u.id, u.name+"@example.test", u.name, u.name); err != nil {
			t.Fatal(err)
		}
	}
	// Ada is the project's admin, Bob a member.
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada.String(), "user_name": "ada", "user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:bob", "dgraph.type": "User", "user_uuid": bob.String(), "user_name": "bob", "user_deleted_at": live, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:P", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Launch", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:ada"}, {"uid": "_:bob"}}, "project_admins": []map[string]any{{"uid": "_:ada"}}},
	})
	as := func(id uuid.UUID, name string) *userModels.UserInfo {
		return &userModels.UserInfo{
			UserPostgresInfo: userModels.User{Id: id, EmailID: name + "@example.test"},
			UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: uids[name], Uuid: id.String(), UserName: name},
		}
	}
	asAda, asBob := as(ada, "ada"), as(bob, "bob")

	// What the web does for one file: the upload, as /uploadFile's sign-in
	// leaves the request (the Postgres account only), then the add, with the
	// card's fields (useUploadFile: a local key in attachment_obj_key).
	add := func(who *userModels.UserInfo, name string) string {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, _ := mw.CreateFormFile("file", name)
		_, _ = part.Write([]byte("the brief"))
		_ = mw.WriteField("jsonData", fmt.Sprintf(`{"src_key":"project","src_value":%q}`, project.String()))
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/uploadFile", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		var signedIn userModels.UserInfo
		signedIn.UserPostgresInfo = who.UserPostgresInfo
		rec := httptest.NewRecorder()
		userController.UploadFile(rec, req.WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, signedIn)))
		var up struct {
			ObjectUUID string `json:"object_uuid"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &up)
		if rec.Code != http.StatusOK || up.ObjectUUID == "" {
			t.Fatalf("%s's upload of %s answered %d: %s", who.UserDgraphInfo.UserName, name, rec.Code, rec.Body.String())
		}
		card, _ := json.Marshal(map[string]any{
			"project_uuid": project.String(),
			"project_attachments": []map[string]any{{
				"attachment_file_name": name, "attachment_obj_key": fmt.Sprint(time.Now().UnixMilli()), "attachment_uuid": up.ObjectUUID,
				"attachment_size": 9, "attachment_created_at": time.Now().UTC().Format(time.RFC3339Nano), "attachment_type": "document",
				"attachment_raw_type": "text/plain",
			}},
		})
		rec = call(t, projectController.AddAttachmentToProjectDgraph, http.MethodPost, "/project/addAttachment", who, string(card), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s's add of %s answered %d: %s", who.UserDgraphInfo.UserName, name, rec.Code, rec.Body.String())
		}
		return up.ObjectUUID
	}
	type listed struct {
		Data struct {
			Attachments []struct {
				UUID string `json:"attachment_uuid"`
				Name string `json:"attachment_file_name"`
			} `json:"project_attachments"`
			Access *struct {
				AddFiles bool `json:"add_files"`
				Manage   bool `json:"manage"`
			} `json:"project_access"`
		} `json:"data"`
	}
	list := func(who *userModels.UserInfo) listed {
		t.Helper()
		rec := call(t, projectController.GetDgraphProjectAttachmentList, http.MethodGet, "/project/attachments/"+project.String(), who, "",
			map[string]string{"project_uuid": project.String()})
		var l listed
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &l) != nil {
			t.Fatalf("the list for %s answered %d: %s", who.UserDgraphInfo.UserName, rec.Code, rec.Body.String())
		}
		return l
	}
	names := func(l listed) map[string]string {
		out := map[string]string{}
		for _, a := range l.Data.Attachments {
			out[a.UUID] = a.Name
		}
		return out
	}
	remove := func(who *userModels.UserInfo, id string) int {
		t.Helper()
		return call(t, projectController.RemoveAttachmentFromProject, http.MethodPost, "/project/removeAttachment", who,
			fmt.Sprintf(`{"attachment_obj_key":%q}`, id), nil).Code
	}

	if l := list(asAda); l.Data.Access == nil || !l.Data.Access.AddFiles || !l.Data.Access.Manage {
		t.Fatalf("the admin's list doesn't say she may add and remove files: %+v", l.Data.Access)
	}
	if l := list(asBob); l.Data.Access == nil || !l.Data.Access.AddFiles || l.Data.Access.Manage {
		t.Fatalf("a member's list should say he may add files and not remove them: %+v", l.Data.Access)
	}

	brief := add(asAda, "brief.txt")
	plan := add(asBob, "plan.txt")
	if got := names(list(asAda)); got[brief] != "brief.txt" || got[plan] != "plan.txt" || len(got) != 2 {
		t.Fatalf("after two adds the project lists %v, want brief.txt (%s) and plan.txt (%s)", got, brief, plan)
	}

	if code := remove(asBob, brief); code != http.StatusForbidden {
		t.Fatalf("a member removing a file answered %d, want 403", code)
	}
	if code := remove(asAda, plan); code != http.StatusOK {
		t.Fatalf("the admin removing a file answered %d, want 200", code)
	}
	if got := names(list(asBob)); got[brief] != "brief.txt" || len(got) != 1 {
		t.Fatalf("after the admin removed plan.txt the project lists %v, want brief.txt alone", got)
	}
}
