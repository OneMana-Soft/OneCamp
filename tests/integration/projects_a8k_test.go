//go:build integration
// +build integration

package integration_test

// Projects A8K (Task Keys #94, Work Taxonomy #95) against real Postgres 12
// and Dgraph, through the handlers people reach: a project made before keys
// gets its key from its name and its tasks their numbers in the order they
// were made; new tasks take the next numbers, never the same one twice; a key
// its admins change keeps answering as an alias; a key opens a task only for
// someone who may open it; every task read carries its key and type; and a
// task's type is changed only by whoever may change the task.
//
// Run: go test -tags=integration ./tests/integration/ -run TestA8K -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskKeysController "github.com/akashc777/OneCamp/controllers/TaskKeys"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestA8KTaskKeysAndTypes(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
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

	// call runs a handler as who, with the route's parameters, a query and a
	// JSON body.
	call := func(who string, h http.HandlerFunc, params map[string]string, query string, body any) (int, string) {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/"+query, rd).WithContext(c))
		return rec.Code, compact(rec.Body.Bytes())
	}
	expect := func(what string, code int, body string, want int, words ...string) {
		t.Helper()
		if code != want {
			t.Errorf("%s: %d %s, want %d", what, code, body, want)
			return
		}
		for _, s := range words {
			if !strings.Contains(body, s) {
				t.Errorf("%s: %s, want it to say %q", what, body, s)
			}
		}
	}
	project := uuid.MustParse(w.project)
	keyOf := func(task string) string {
		t.Helper()
		facts, err := taskKeyModel.FactsFor(ctx, []uuid.UUID{uuid.MustParse(task)})
		if err != nil {
			t.Fatal(err)
		}
		return facts[uuid.MustParse(task)].Key
	}

	t.Run("a project made before keys gets its key and its tasks their numbers, oldest first", func(t *testing.T) {
		if got := keyOf(w.task); got != "" {
			t.Fatalf("before keys, a task read as %q", got)
		}
		if err := taskKeys.Settle(ctx, project); err != nil {
			t.Fatal(err)
		}
		key, next, _, err := taskKeys.Of(ctx, project)
		if err != nil || key != "WEB" || next != 5 {
			t.Fatalf("Website's key %q, next %d, %v; want WEB, 5", key, next, err)
		}
		for i, task := range []string{w.task, w.assigned, w.mine, w.deleted} {
			if got, want := keyOf(task), fmt.Sprintf("WEB-%d", i+1); got != want {
				t.Errorf("task %d reads %q, want %q", i, got, want)
			}
		}
	})

	t.Run("tasks made at once each take the next number, never the same one", func(t *testing.T) {
		const n = 20
		got := make([]int, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				num, err := taskKeyModel.InsertTask(ctx, uuid.New(), project, w.people["ana"], time.Now())
				if err != nil {
					t.Errorf("task %d: %v", i, err)
				}
				got[i] = num
			}(i)
		}
		wg.Wait()
		seen := map[int]bool{}
		for _, num := range got {
			if num < 5 || num > 4+n || seen[num] {
				t.Errorf("numbers %v; want %d to %d, each once", got, 5, 4+n)
				break
			}
			seen[num] = true
		}
	})

	t.Run("a key its admins change keeps finding its tasks", func(t *testing.T) {
		code, body := call("ben", taskKeysController.SetProjectKey, map[string]string{"project_uuid": w.project}, "", map[string]string{"key": "SITE"})
		expect("a member changing the key", code, body, http.StatusForbidden, "Only the project's admins change its key.")
		code, body = call("ana", taskKeysController.SetProjectKey, map[string]string{"project_uuid": w.project}, "", map[string]string{"key": "1ab"})
		expect("a key starting with a digit", code, body, http.StatusBadRequest, "starts with a letter")
		code, body = call("ana", taskKeysController.SetProjectKey, map[string]string{"project_uuid": w.project}, "", map[string]string{"key": "site"})
		expect("an admin changing the key", code, body, http.StatusOK, `"key":"SITE"`, `"aliases":["WEB"]`)
		for _, raw := range []string{"SITE-1", "web-1"} {
			if id, err := taskKeys.Resolve(ctx, raw); err != nil || id.String() != w.task {
				t.Errorf("%s finds %s, %v; want the first task", raw, id, err)
			}
		}

		// Another project can't take a key in use, or one Website used.
		other := uuid.New()
		w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Ops', $2, $3)`, other, w.team, w.people["ana"])
		if k, err := taskKeys.Ensure(ctx, other, "Ops"); err != nil || k != "OPS" {
			t.Fatalf("Ops' key %q, %v", k, err)
		}
		for _, k := range []string{"SITE", "WEB"} {
			if _, _, err := taskKeys.SetKey(ctx, other, k, w.people["ana"]); err != taskKeys.ErrKeyTaken {
				t.Errorf("Ops taking %s: %v; want ErrKeyTaken", k, err)
			}
		}
		// Website takes its old key back.
		if old, now, err := taskKeys.SetKey(ctx, project, "WEB", w.people["ana"]); err != nil || old != "SITE" || now != "WEB" {
			t.Errorf("taking WEB back: %q → %q, %v", old, now, err)
		}
		if id, err := taskKeys.Resolve(ctx, "SITE-2"); err != nil || id.String() != w.assigned {
			t.Errorf("SITE-2 after WEB came back: %s, %v", id, err)
		}
	})

	t.Run("a key opens a task only for someone who may open it", func(t *testing.T) {
		code, body := call("ben", taskKeysController.ResolveTask, map[string]string{"ref": "web-1"}, "", nil)
		expect("a member", code, body, http.StatusOK, `"task_uuid":"`+w.task+`"`, `"task_key":"WEB-1"`)
		code, body = call("eve", taskKeysController.ResolveTask, map[string]string{"ref": "WEB-2"}, "", nil)
		expect("the assignee from outside the project", code, body, http.StatusOK, `"task_key":"WEB-2"`)
		for _, c := range []struct{ who, ref string }{{"cy", "WEB-1"}, {"ben", "WEB-999"}, {"ben", "NOPE-1"}} {
			code, body = call(c.who, taskKeysController.ResolveTask, map[string]string{"ref": c.ref}, "", nil)
			expect(c.who+" opening "+c.ref, code, body, http.StatusNotFound, "that you can open")
		}
	})

	t.Run("every task read carries its key", func(t *testing.T) {
		code, body := call("ben", taskController.GetTaskInfo, map[string]string{"task_uuid": w.task}, "", nil)
		expect("the panel", code, body, http.StatusOK, `"task_key":"WEB-1"`)
		code, body = call("ben", projectController.GetProjectTaskList, map[string]string{"project_uuid": w.project}, "?pageSize=50&pageIndex=0", nil)
		expect("the list", code, body, http.StatusOK, `"task_key":"WEB-1"`, `"task_key":"WEB-3"`)
		code, body = call("ben", projectController.GetProjectTaskListForKanban, map[string]string{"project_uuid": w.project}, "", nil)
		expect("the board", code, body, http.StatusOK, `"task_key":"WEB-1"`)
		code, body = call("ben", userController.GetDgraphUserTaskList, nil, "?pageSize=50&pageIndex=0", nil)
		expect("My Tasks", code, body, http.StatusOK, `"task_key":"WEB-3"`)
		// A task route takes the key as its id.
		code, body = call("ben", taskKeysController.SetTaskType, nil, "", map[string]string{"task_uuid": "web-1", "type_id": workTaxonomy.TypeBug.String()})
		expect("a type set through the key", code, body, http.StatusOK, `"builtin":"bug"`)
	})

	t.Run("a task's type is changed by whoever may change the task", func(t *testing.T) {
		code, body := call("ben", taskKeysController.ProjectTypes, map[string]string{"project_uuid": w.project}, "", nil)
		expect("the types", code, body, http.StatusOK, `"builtin":"milestone"`, `"builtin":"approval"`, `"icons":[`)
		code, body = call("ben", taskKeysController.CreateProjectType, map[string]string{"project_uuid": w.project}, "", map[string]string{"name": "Spike", "icon": "flask"})
		expect("a member adding a type", code, body, http.StatusForbidden, "Only the project's admins add its types.")
		code, body = call("ana", taskKeysController.CreateProjectType, map[string]string{"project_uuid": w.project}, "", map[string]string{"name": "Spike", "icon": "flask"})
		expect("an admin adding a type", code, body, http.StatusCreated, `"name":"Spike"`)
		var made struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &made)
		code, body = call("ana", taskKeysController.CreateProjectType, map[string]string{"project_uuid": w.project}, "", map[string]string{"name": "spike", "icon": "flask"})
		expect("the same name again", code, body, http.StatusConflict, "already has a type of that name")

		code, body = call("ben", taskKeysController.SetTaskType, nil, "", map[string]string{"task_uuid": w.mine, "type_id": made.Data.ID})
		expect("a member giving a task the project's type", code, body, http.StatusOK, `"name":"Spike"`)
		code, body = call("ben", taskController.GetTaskInfo, map[string]string{"task_uuid": w.mine}, "", nil)
		expect("the panel after", code, body, http.StatusOK, `"task_work_type":{`, `"name":"Spike"`, `"activity_type":"typeUpdate"`)

		// Another project's type isn't this one's.
		other := uuid.New()
		w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Elsewhere', $2, $3)`, other, w.team, w.people["ana"])
		theirs, err := workTaxonomy.Create(ctx, other, "Theirs", "flag", w.people["ana"])
		if err != nil {
			t.Fatal(err)
		}
		code, body = call("ben", taskKeysController.SetTaskType, nil, "", map[string]string{"task_uuid": w.mine, "type_id": theirs.ID.String()})
		expect("another project's type", code, body, http.StatusNotFound, "isn't one of this project's")

		// Archived: the task keeps it, the pickers don't offer it.
		code, body = call("ana", taskKeysController.ArchiveProjectType, map[string]string{"project_uuid": w.project, "type_id": made.Data.ID}, "", nil)
		expect("archiving the type", code, body, http.StatusOK)
		code, body = call("ben", taskKeysController.ProjectTypes, map[string]string{"project_uuid": w.project}, "", nil)
		if strings.Contains(body, "Spike") {
			t.Errorf("an archived type is still offered: %s", body)
		}
		code, body = call("ben", taskController.GetTaskInfo, map[string]string{"task_uuid": w.mine}, "", nil)
		expect("its task after", code, body, http.StatusOK, `"name":"Spike"`)

		// Back to a plain Task; and nobody outside the project changes it.
		code, body = call("ben", taskKeysController.SetTaskType, nil, "", map[string]string{"task_uuid": w.mine, "type_id": ""})
		expect("back to a Task", code, body, http.StatusOK, `"task_work_type":null`)
		code, body = call("cy", taskKeysController.SetTaskType, nil, "", map[string]string{"task_uuid": w.mine, "type_id": workTaxonomy.TypeBug.String()})
		expect("someone outside the project", code, body, http.StatusNotFound)
	})

	t.Run("keys and types never reach the graph", func(t *testing.T) {
		got := string(w.dg.Query(t, fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { expand(_all_) } }`, w.task)))
		for _, leak := range []string{"task_key", "task_work_type", "WEB-1"} {
			if strings.Contains(got, leak) {
				t.Errorf("the graph's task holds %q: %s", leak, got)
			}
		}
	})
}

// A key typed into search finds its task first, for the people in its
// project, and nobody else finds it by its key.
func TestA8KAKeyTypedIntoSearchFindsItsTask(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	if err := taskKeys.Settle(ctx, uuid.MustParse(w.project)); err != nil {
		t.Fatal(err)
	}
	website := []*dgraphStruct.DgraphProject{{Uuid: w.project}}
	page, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, accessBusiness.Subject{UserUUID: w.people["ben"].String()}, "ben@acme.test", nil, website, nil, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Page) == 0 || page.Page[0].Task == nil || page.Page[0].Task.Uuid != w.task || page.Page[0].Task.TaskKey != "WEB-1" {
		t.Fatalf("ben's search for web-1: %+v; want the task first, as WEB-1", page.Page)
	}
	page, err = globalSearchBusiness.GetUnifiedGlobalSearch(ctx, accessBusiness.Subject{UserUUID: w.people["cy"].String()}, "cy@acme.test", nil, nil, nil, "WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range page.Page {
		if h.Task != nil && h.Task.Uuid == w.task {
			t.Errorf("cy, outside the project, found it: %+v", h.Task)
		}
	}
}
