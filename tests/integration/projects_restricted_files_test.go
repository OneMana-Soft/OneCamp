//go:build integration
// +build integration

package integration_test

// A restricted task's files go only to the people who may read the task (its
// project's admins, its assignee, its maker and the people let in): its own
// files and its comments', uploaded in the app or brought by an import. They
// are found in search and served through the project's file route (by id,
// and as the picture a page shows, ?redirect=1) to those people and to nobody
// else in the project, and the project's Files tab doesn't list them.
//
// seedWorkWorld's "Write the launch post" is ana's, in Website, which she
// runs with fay; ben is a member, and cy is in the team but not the project.
// Ben gets the task's files until ana keeps the task to its admins and its
// maker; from then on he gets what a file that isn't there gets. "Send the
// newsletter", open to the project, is the control: ben still gets its
// files, the one uploaded in the app and the one an import brought.
//
// Run: go test -tags=integration ./tests/integration/ -run TestARestrictedTasksFilesGoOnlyToItsReaders -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userController "github.com/akashc777/OneCamp/controllers/User"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestARestrictedTasksFilesGoOnlyToItsReaders(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	startMinio(t)

	// The graph's uid of a node, by its uuid predicate.
	uidOf := func(pred, id string) string {
		t.Helper()
		var out struct {
			Q []struct {
				UID string `json:"uid"`
			} `json:"q"`
		}
		if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(%s, %q)) { uid } }`, pred, id)), &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("the node with %s %s: %+v %v", pred, id, out, err)
		}
		return out.Q[0].UID
	}
	website, team := uidOf("project_uuid", w.project), uidOf("team_uuid", w.team)
	launch, newsletter := uidOf("task_uuid", w.task), uidOf("task_uuid", w.mine)
	person := func(name string) string { return uidOf("user_uuid", w.people[name].String()) }

	// Who is in the project and the team, as signing in reads it.
	var nodes []map[string]any
	for _, name := range []string{"ana", "ben", "fay"} {
		nodes = append(nodes, map[string]any{"uid": person(name), "user_projects": []map[string]any{{"uid": website}}, "user_teams": []map[string]any{{"uid": team}}})
	}
	nodes = append(nodes, map[string]any{"uid": person("cy"), "user_teams": []map[string]any{{"uid": team}}})

	// The files: the task's own and one on its comment, uploaded in the app
	// (kept under the project) and brought by an import (kept under the task
	// and the comment), and the open task's two.
	type file struct{ id, name, kind, src string }
	comment := uuid.NewString()
	own := file{uuid.NewString(), "zephyr-pay-bands.xlsx", "project", w.project}
	onComment := file{uuid.NewString(), "zephyr-offer-letter.pdf", "project", w.project}
	imported := file{uuid.NewString(), "zephyr-import-notes.docx", "task", w.task}
	importedOnComment := file{uuid.NewString(), "zephyr-import-reply.pdf", "comment", comment}
	open := file{uuid.NewString(), "newsletter-draft.pdf", "project", w.project}
	importedOpen := file{uuid.NewString(), "newsletter-import.pdf", "task", w.mine}
	kept := []file{own, onComment, imported, importedOnComment}
	opened := []file{open, importedOpen}
	live := "0001-01-01T00:00:00Z"
	now := time.Now().UTC()
	node := func(f file) map[string]any {
		return map[string]any{"dgraph.type": "Attachment", "attachment_uuid": f.id, "attachment_file_name": f.name,
			"attachment_obj_key": "userFileUpload/" + f.name, "attachment_deleted_at": live}
	}
	nodes = append(nodes,
		map[string]any{"uid": launch, "task_attachments": []map[string]any{node(own), node(imported)},
			"task_comments": []map[string]any{{"dgraph.type": "Comment", "comment_uuid": comment,
				"comment_text": "<p>The letters are attached.</p>", "comment_task": map[string]any{"uid": launch},
				"comment_by": map[string]any{"uid": person("ana")}, "comment_created_at": now.Format(time.RFC3339),
				"comment_deleted_at": live, "comment_attachments": []map[string]any{node(onComment), node(importedOnComment)}}}},
		map[string]any{"uid": newsletter, "task_attachments": []map[string]any{node(open), node(importedOpen)}})
	w.dg.Mutate(t, nodes)
	for _, f := range append(kept, opened...) {
		w.exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`,
			f.id, "userFileUpload/"+f.name, f.kind, f.src, w.people["ana"])
	}
	w.exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, comment, w.people["ana"])

	// Indexed as attaching them indexes them.
	ana := &dgraphStruct.DgraphUser{Uuid: w.people["ana"].String(), UserName: "ana"}
	attachment := func(f file) *dgraphStruct.DgraphAttachment {
		return &dgraphStruct.DgraphAttachment{Uuid: f.id, FileName: f.name, ObjectKey: "userFileUpload/" + f.name}
	}
	inWebsite := func(task string) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{Uuid: task, Project: &dgraphStruct.DgraphProject{Uuid: w.project, Name: "Website"}}
	}
	taskDomain.CreateTaskAttachmentsInOpensearch(&now, inWebsite(w.task), []*dgraphStruct.DgraphAttachment{attachment(own), attachment(imported)}, ana)
	taskDomain.CreateTaskAttachmentsInOpensearch(&now, inWebsite(w.mine), []*dgraphStruct.DgraphAttachment{attachment(open), attachment(importedOpen)}, ana)
	commentDomain.CreateTaskCommentWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchComment{Uuid: comment,
		CommentBody: "The letters are attached.", CommentByUserUuid: ana.Uuid, CommentByUserFullName: "ana",
		CommentProjectUuid: w.project, CommentProjectName: "Website", CommentTaskUuid: w.task, CommentCreatedAt: now.Unix()},
		[]*dgraphStruct.DgraphAttachment{attachment(onComment), attachment(importedOnComment)})

	// as is who, signed in, as the first middleware leaves a request.
	as := func(who string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people[who])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", who, err)
		}
		gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, w.people[who].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's profile: %v", who, err)
		}
		return taskread.Install(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}))
	}
	get := func(who string, h http.HandlerFunc, params map[string]string, query string) (int, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/"+query, nil).WithContext(context.WithValue(as(who), chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}
	// fetch asks the project's file route for a file, as the task panel, a
	// comment's files and a search result open one.
	fetch := func(who string, f file, query string) (int, string) {
		t.Helper()
		return get(who, userController.GetProjectFile, map[string]string{"project_uuid": w.project, "obj_uuid": f.id}, query)
	}
	searches := map[string]func(who string) string{
		"the app's search": func(who string) string {
			raw, _ := json.Marshal(map[string]string{"global_search_text": "zephyr"})
			rec := httptest.NewRecorder()
			globalSearchController.GetUnifiedGlobalSearch(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(who)))
			return rec.Body.String()
		},
		"the public API's search": func(who string) string {
			rec := httptest.NewRecorder()
			v1Controller.SearchV1(rec, httptest.NewRequest(http.MethodGet, "/?q=zephyr", nil).WithContext(as(who)))
			return rec.Body.String()
		},
	}
	// found is which of the files an answer names, by name or id.
	found := func(body string, fs ...file) []string {
		var got []string
		for _, f := range fs {
			if strings.Contains(body, f.name) || strings.Contains(body, f.id) {
				got = append(got, f.name)
			}
		}
		return got
	}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out waiting until %s", what)
	}

	// Before: ben may read the task, so he finds its files and gets them. The
	// legs below would see them if they leaked.
	for name, run := range searches {
		eventually("ben finds the task's files through "+name, func() bool { return len(found(run("ben"), kept...)) == len(kept) })
	}
	for _, f := range kept {
		if code, out := fetch("ben", f, ""); code != http.StatusOK {
			t.Fatalf("before the task is restricted, ben gets %s: %d %s", f.name, code, out)
		}
	}

	// Ana keeps the task to its admins and its maker.
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, person("ana"))
	if err != nil || task == nil {
		t.Fatalf("read the task: %v", err)
	}
	if err := workAccess.Restrict(ctx, task, w.people["ana"], nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	missingCode, missing := fetch("ben", file{id: uuid.NewString()}, "")

	t.Run("a member it is kept from gets none of its files", func(t *testing.T) {
		for name, run := range searches {
			if got := found(run("ben"), kept...); len(got) != 0 {
				t.Errorf("ben finds %v through %s", got, name)
			}
		}
		for _, f := range kept {
			for _, query := range []string{"", "?redirect=1"} {
				if code, out := fetch("ben", f, query); code != missingCode || out != missing {
					t.Errorf("ben asks for %s%s: %d %s; want what a file that isn't there gets: %d %s", f.name, query, code, out, missingCode, missing)
				}
			}
		}
		for _, f := range opened {
			if code, out := fetch("ben", f, ""); code != http.StatusOK {
				t.Errorf("ben is refused the open task's %s: %d %s", f.name, code, out)
			}
		}
		if _, tab := get("ben", projectController.GetDgraphProjectAttachmentList, map[string]string{"project_uuid": w.project}, ""); len(found(tab, kept...)) != 0 {
			t.Errorf("the project's Files tab lists %v to ben", found(tab, kept...))
		}
	})

	t.Run("its readers get every one of them", func(t *testing.T) {
		for _, who := range []string{"ana", "fay"} {
			for name, run := range searches {
				if got := found(run(who), kept...); len(got) != len(kept) {
					t.Errorf("%s finds only %v through %s", who, got, name)
				}
			}
			for _, f := range kept {
				if code, out := fetch(who, f, ""); code != http.StatusOK {
					t.Errorf("%s asks for %s: %d %s", who, f.name, code, out)
				}
				if code, out := fetch(who, f, "?redirect=1"); code != http.StatusFound {
					t.Errorf("%s opens %s as a picture: %d %s", who, f.name, code, out)
				}
			}
		}
	})

	t.Run("someone outside the project gets none of the project's files", func(t *testing.T) {
		for name, run := range searches {
			if got := found(run("cy"), kept...); len(got) != 0 {
				t.Errorf("cy finds %v through %s", got, name)
			}
		}
		for _, f := range append(kept, opened...) {
			if code, _ := fetch("cy", f, ""); code != http.StatusForbidden {
				t.Errorf("cy asks for %s: %d, want 403", f.name, code)
			}
		}
	})
}
