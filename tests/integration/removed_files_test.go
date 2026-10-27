//go:build integration
// +build integration

package integration_test

// A file taken off a task or a project, a file kept under a comment that was
// deleted, and a deleted comment's own file open for nobody: every read of it
// by id answers what a file that isn't there answers (404), to the project's
// members as to its admins, as a link and as the picture a page shows
// (?redirect=1). A file nobody removed still opens.
//
// seedWorkWorld's Website: ana runs it, ben is its member, cy is in its team
// but not the project. "Write the launch post" is ana's.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestARemovedFileOpensForNobody$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestARemovedFileOpensForNobody(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	startMinio(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"deleted":0,"failures":[],"result":"updated","_id":"x","hits":{"total":{"value":0},"hits":[]}}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

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
	launch := uidOf("task_uuid", w.task)
	website := uidOf("project_uuid", w.project)
	ana := uidOf("user_uuid", w.people["ana"].String())

	// The launch post's file and the project's, uploaded in the app (kept
	// under the project), a comment on the post with a file of its own (a task
	// comment's upload is the project's too) and one an import kept under it,
	// and one more of the post's that nobody removes.
	type file struct{ id, name, kind, src string }
	comment := uuid.NewString()
	onTask := file{uuid.NewString(), "wrong-draft.pdf", "project", w.project}
	onProject := file{uuid.NewString(), "old-brief.pdf", "project", w.project}
	onComment := file{uuid.NewString(), "imported-reply.pdf", "comment", comment}
	commentsOwn := file{uuid.NewString(), "reply-sketch.png", "project", w.project}
	kept := file{uuid.NewString(), "final-draft.pdf", "project", w.project}
	live := "0001-01-01T00:00:00Z"
	node := func(f file) map[string]any {
		return map[string]any{"dgraph.type": "Attachment", "attachment_uuid": f.id, "attachment_file_name": f.name,
			"attachment_obj_key": "userFileUpload/" + f.name, "attachment_deleted_at": live}
	}
	w.dg.Mutate(t, []map[string]any{
		{"uid": launch, "task_attachments": []map[string]any{node(onTask), node(kept)},
			"task_comments": []map[string]any{{"dgraph.type": "Comment", "comment_uuid": comment,
				"comment_text": "<p>Attached, from the old tracker.</p>", "comment_task": map[string]any{"uid": launch},
				"comment_by": map[string]any{"uid": ana}, "comment_created_at": time.Now().UTC().Format(time.RFC3339),
				"comment_deleted_at": live, "comment_attachments": []map[string]any{node(onComment), node(commentsOwn)}}}},
		{"uid": website, "project_attachments": []map[string]any{node(onProject)}},
	})
	for _, f := range []file{onTask, onProject, onComment, commentsOwn, kept} {
		w.exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`,
			f.id, "userFileUpload/"+f.name, f.kind, f.src, w.people["ana"])
	}
	w.exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, comment, w.people["ana"])

	call := func(who string, h http.HandlerFunc, params map[string]string, target string, body any) (int, string) {
		t.Helper()
		ctx, _ := w.signedIn(who)
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, target, bytes.NewReader(raw)).WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc)))
		out := rec.Body.String()
		if len(out) > 200 {
			out = out[:200]
		}
		return rec.Code, out
	}
	fetch := func(who string, f file, query string) (int, string) {
		t.Helper()
		return call(who, userController.GetProjectFile, map[string]string{"project_uuid": w.project, "obj_uuid": f.id}, "/"+query, nil)
	}

	// Before: the project's people open all five; cy, outside it, none.
	for _, f := range []file{onTask, onProject, onComment, commentsOwn, kept} {
		for _, who := range []string{"ana", "ben"} {
			if code, body := fetch(who, f, ""); code != http.StatusOK {
				t.Fatalf("before: %s opening %s: %d %s", who, f.name, code, body)
			}
		}
		if code, body := fetch("cy", f, ""); code == http.StatusOK {
			t.Fatalf("before: cy, outside the project, opened %s: %s", f.name, body)
		}
	}

	// ana takes one file off the post and one off the project, and deletes her
	// comment, the way the app does.
	if code, body := call("ana", taskController.RemoveAttachmentFromTask, nil, "/",
		map[string]string{"attachment_obj_key": onTask.id, "task_uuid": w.task}); code != http.StatusOK {
		t.Fatalf("ana taking %s off the post: %d %s", onTask.name, code, body)
	}
	if code, body := call("ana", projectController.RemoveAttachmentFromProject, nil, "/",
		map[string]string{"attachment_obj_key": onProject.id, "project_uuid": w.project}); code != http.StatusOK {
		t.Fatalf("ana taking %s off the project: %d %s", onProject.name, code, body)
	}
	if code, body := call("ana", taskController.DeleteTaskComment, nil, "/",
		map[string]string{"task_comment_uuid": comment, "task_uuid": w.task}); code != http.StatusOK {
		t.Fatalf("ana deleting her comment: %d %s", code, body)
	}

	// After: each removed file is no such file, to its member and its admin,
	// as a link and as a picture; the file nobody removed still opens.
	for _, f := range []file{onTask, onProject, onComment, commentsOwn} {
		for _, who := range []string{"ben", "ana"} {
			for _, query := range []string{"", "?redirect=1"} {
				if code, body := fetch(who, f, query); code != http.StatusNotFound {
					t.Errorf("%s opened %s%s after it was removed: %d %s", who, f.name, query, code, body)
				}
			}
		}
	}
	if code, body := fetch("ben", kept, ""); code != http.StatusOK {
		t.Errorf("ben opening %s, which nobody removed: %d %s", kept.name, code, body)
	}
}
