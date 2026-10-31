//go:build integration

package integration_test

// P7-1b: a task, a sub-task or a comment sent again under its Idempotency-Key
// is made once (router.TaskCreates), as the web's Offline Store sends a change
// made offline again when the network comes back. The key is the member's
// own: another member sending the same key, with the same body, makes theirs
// and is never given the first member's answer. Without a key every send
// makes one, as before. Through the routes as router/ mounts them, against
// real Postgres, Redis and Dgraph.
//
// Run: PKGS=./tests/integration/ ... -run 'TestP7ACreateSentAgainUnderItsKeyIsMadeOnce'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestP7ACreateSentAgainUnderItsKeyIsMadeOnce(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	// The task routes as router/ mounts them, after a sign-in stand-in that
	// puts the member named in X-Test-Member on the request, as VerifyAuth does.
	tasks := chi.NewRouter()
	tasks.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, w.account(r.Header.Get("X-Test-Member")))))
		})
	})
	router.TaskCreates(tasks)

	send := func(who, path, key string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Member", who)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		tasks.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	made := func(out map[string]any) string {
		data, _ := out["data"].(map[string]any)
		id, _ := data["task_uuid"].(string)
		return id
	}
	taskCount := func() int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM tasks WHERE project_id = $1`, w.project).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	commentCount := func() int {
		t.Helper()
		var out struct {
			Q []struct {
				N int `json:"n"`
			} `json:"q"`
		}
		if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { n: count(task_comments) } }`, w.task)), &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("the task's comments: %+v %v", out, err)
		}
		return out.Q[0].N
	}

	// A task.
	venue := map[string]any{"task_project_uuid": w.project, "task_name": "Book the venue"}
	before := taskCount()
	first, a := send("ana", "/createTask", "offline-1", venue)
	if first.Code != http.StatusOK || made(a) == "" {
		t.Fatalf("ana's first send answered %d: %v", first.Code, a)
	}
	again, b := send("ana", "/createTask", "offline-1", venue)
	if again.Code != http.StatusOK || again.Header().Get("Idempotent-Replayed") != "true" || made(b) != made(a) {
		t.Errorf("ana's send again under its key answered %d %v (replayed %q), the first %v", again.Code, b, again.Header().Get("Idempotent-Replayed"), a)
	}
	if n := taskCount() - before; n != 1 {
		t.Errorf("one key made %d tasks, want 1", n)
	}

	// Another member with the same key and body: theirs, never ana's answer.
	bens, c := send("ben", "/createTask", "offline-1", venue)
	if bens.Code != http.StatusOK || bens.Header().Get("Idempotent-Replayed") == "true" || made(c) == "" || made(c) == made(a) {
		t.Errorf("ben's send under ana's key answered %d %v (replayed %q); ana's task %s", bens.Code, c, bens.Header().Get("Idempotent-Replayed"), made(a))
	}
	if n := taskCount() - before; n != 2 {
		t.Errorf("ana's send twice and ben's once made %d tasks, want 2", n)
	}

	// A sub-task.
	quotes := map[string]any{"task_uuid": w.task, "task_project_uuid": w.project, "task_name": "Get three quotes"}
	before = taskCount()
	send("ana", "/createSubTask", "offline-2", quotes)
	sub, _ := send("ana", "/createSubTask", "offline-2", quotes)
	if sub.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("a sub-task sent again under its key answered %d without replaying", sub.Code)
	}
	if n := taskCount() - before; n != 1 {
		t.Errorf("one key made %d sub-tasks, want 1", n)
	}

	// A comment.
	note := map[string]any{"task_uuid": w.task, "task_comment_body": "<p>Venue booked for the 12th</p>"}
	comments := commentCount()
	send("ana", "/createCommentTask", "offline-3", note)
	re, _ := send("ana", "/createCommentTask", "offline-3", note)
	if re.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("a comment sent again under its key answered %d without replaying", re.Code)
	}
	if n := commentCount() - comments; n != 1 {
		t.Errorf("one key made %d comments, want 1", n)
	}

	// Without a key, every send makes one, as before.
	before = taskCount()
	send("ana", "/createTask", "", venue)
	send("ana", "/createTask", "", venue)
	if n := taskCount() - before; n != 2 {
		t.Errorf("two sends without a key made %d tasks, want 2", n)
	}

	// A member the project doesn't have is refused, key or not, and nothing is made.
	before = taskCount()
	eve, out := send("eve", "/createTask", "offline-4", venue)
	if eve.Code < 400 || eve.Code >= 500 {
		t.Errorf("eve, outside the project and its team, answered %d %v", eve.Code, out)
	}
	if n := taskCount() - before; n != 0 {
		t.Errorf("eve's refused send made %d tasks", n)
	}
}
