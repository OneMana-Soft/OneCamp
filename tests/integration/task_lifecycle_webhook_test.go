//go:build integration

package integration_test

// linear-39 round 2 (main, 1 Nov) through the handlers: a task made, changed,
// deleted and restored reaches webhooks through the one path every task change
// takes, the Work Change Log's entry and its Outbox event written together
// (business/WorkFeed sinks.go), against real Postgres 12 with every migration:
//   - each change writes one Outbox event of its own kind, in order;
//   - the endpoint is sent each event once, with the event's own id as its
//     delivery id, and nothing else (no second send from a path of its own);
//   - task.created, task.deleted and task.restored keep the keys they were
//     always sent with, and name the person by their uuid;
//   - a task.updated says what each changed field was before ("previous",
//     Linear's updatedFrom), read from the log's earlier entries of the task;
//   - a sub-task's making is sent too (main, 1 Nov: every change), through
//     the same path, as task.created naming its parent (parent_task_id).
//
// Run: go test -tags=integration ./tests/integration/ -run TestATaskMadeChangedDeletedAndRestoredReachesWebhooksThroughTheOutbox -v

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestATaskMadeChangedDeletedAndRestoredReachesWebhooksThroughTheOutbox(t *testing.T) {
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
	defer hooksBusiness.SendToThisNetworkForTest()()

	// The endpoint keeps what it is sent.
	type sent struct {
		Delivery string
		Event    string         `json:"event"`
		EventID  string         `json:"event_id"`
		Data     map[string]any `json:"data"`
	}
	var mu sync.Mutex
	var got []sent
	endpoint := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var s sent
		if err := json.Unmarshal(b, &s); err != nil {
			t.Errorf("body %s: %v", b, err)
		}
		s.Delivery = r.Header.Get("X-OneCamp-Delivery")
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
		rw.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(endpoint.Close)

	// Ana, an admin of the project, sets a webhook up for its tasks' every change.
	w.exec(`INSERT INTO webhooks (id, name, type, token, secret, target_url, events, is_active, created_by, bot_name, scope_type, scope_entity_id, created_at, updated_at)
		VALUES ($1, 'Tasks', 'outgoing', $2, 'shh', $3, '["task.created","task.updated","task.deleted","task.restored"]', true, $4, 'Tasks', 'project', $5, NOW(), NOW())`,
		uuid.New(), uuid.NewString(), endpoint.URL, w.people["ana"], w.project)
	ana := w.people["ana"].String()

	// The task's life, through the app's own routes.
	if code := pastCall(t, taskController.CreateTask, http.MethodPost, "/",
		map[string]any{"task_project_uuid": w.project, "task_name": "Release notes", "task_description": "Draft them", "task_priority": "low"}, w.account("ana"), nil, nil); code != http.StatusOK {
		t.Fatalf("create: %d", code)
	}
	var task string
	if err := w.env.PG.QueryRow(`SELECT resource_uuid::text FROM task_changes WHERE kind = 'task.created' AND project_uuid = $1 ORDER BY sequence DESC LIMIT 1`, w.project).Scan(&task); err != nil {
		t.Fatalf("the made task isn't in the log: %v", err)
	}
	for _, p := range []string{"medium", "high"} {
		if code := pastCall(t, taskController.UpdateTaskPriority, http.MethodPost, "/",
			map[string]any{"task_uuid": task, "task_priority": p, "task_project_uuid": w.project}, w.account("ana"), nil, nil); code != http.StatusOK {
			t.Fatalf("priority %s: %d", p, code)
		}
	}
	if code := pastCall(t, taskController.ArchiveTask, http.MethodPost, "/", map[string]any{"task_uuid": task, "task_project_uuid": w.project}, w.account("ana"), nil, nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if code := pastCall(t, taskController.UnArchiveTask, http.MethodPost, "/", map[string]any{"task_uuid": task, "task_project_uuid": w.project}, w.account("ana"), nil, nil); code != http.StatusOK {
		t.Fatalf("restore: %d", code)
	}

	// Each change wrote one event of its own kind with its entry, in order.
	want := []string{"task.created", "task.updated", "task.updated", "task.deleted", "task.restored"}
	rows, err := w.env.PG.Query(`SELECT kind, event_id::text FROM outbox WHERE topic = 'work.changes' AND key = $1 ORDER BY id`, "task:"+task)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	ids := map[string]string{}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
		ids[id] = k
	}
	rows.Close()
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("the task's events in the outbox are %v, want %v: every change through the one path", kinds, want)
	}

	// The endpoint is sent each event once, by its own id, and nothing else.
	mine := func() []sent {
		mu.Lock()
		defer mu.Unlock()
		var out []sent
		for _, s := range got {
			if s.Data["task_id"] == task {
				out = append(out, s)
			}
		}
		return out
	}
	for deadline := time.Now().Add(20 * time.Second); len(mine()) < len(want) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(700 * time.Millisecond)
	deliveries := mine()
	var order []string
	for _, s := range deliveries {
		order = append(order, s.Event)
		if ids[s.Delivery] != s.Event || s.EventID != s.Delivery {
			t.Errorf("%s was sent with delivery id %q (event_id %q), which is no %s event of the outbox's", s.Event, s.Delivery, s.EventID, s.Event)
		}
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("the endpoint was sent %v, want %v, each once", order, want)
	}

	// What each says.
	made, first, second, deleted, restored := deliveries[0].Data, deliveries[1].Data, deliveries[2].Data, deliveries[3].Data, deliveries[4].Data
	if made["project_id"] != w.project || made["name"] != "Release notes" || made["description"] != "Draft them" ||
		made["priority"] != "low" || made["status"] == nil || made["created_by"] != ana {
		t.Errorf("task.created said %v, want its name, description, status, priority low and created_by ana", made)
	}
	priority := func(d map[string]any, key string) any { m, _ := d[key].(map[string]any); return m["task_priority"] }
	if priority(first, "changes") != "medium" || priority(first, "previous") != "low" {
		t.Errorf("the first change said %v, want task_priority medium, previously low", first)
	}
	if priority(second, "changes") != "high" || priority(second, "previous") != "medium" {
		t.Errorf("the second change said %v, want task_priority high, previously medium", second)
	}
	if deleted["name"] != "Release notes" || deleted["status"] == nil || deleted["deleted_by"] != ana {
		t.Errorf("task.deleted said %v, want its name, status and deleted_by ana", deleted)
	}
	if restored["name"] != "Release notes" || restored["restored_by"] != ana {
		t.Errorf("task.restored said %v, want its name and restored_by ana", restored)
	}
	if _, named := made["parent_task_id"]; named {
		t.Errorf("a top-level task's task.created named a parent: %v", made)
	}

	// A sub-task's making reaches the endpoint too (main, 1 Nov: every change),
	// through the same path, as task.created naming its parent.
	var subMade struct {
		Uuid string `json:"task_uuid"`
	}
	if code := pastCall(t, taskController.CreateSubTask, http.MethodPost, "/",
		map[string]any{"task_uuid": task, "task_project_uuid": w.project, "task_name": "Collect the changes"}, w.account("ana"), nil, &subMade); code != http.StatusOK || subMade.Uuid == "" {
		t.Fatalf("sub-task: %d %+v", code, subMade)
	}
	sub := subMade.Uuid
	var subKinds, subIDs []string
	rows, err = w.env.PG.Query(`SELECT kind, event_id::text FROM outbox WHERE topic = 'work.changes' AND key = $1 ORDER BY id`, "task:"+sub)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			t.Fatal(err)
		}
		subKinds, subIDs = append(subKinds, k), append(subIDs, id)
	}
	rows.Close()
	if !reflect.DeepEqual(subKinds, []string{"task.created"}) {
		t.Fatalf("the sub-task's events in the outbox are %v, want [task.created]", subKinds)
	}
	ofSub := func() []sent {
		mu.Lock()
		defer mu.Unlock()
		var out []sent
		for _, s := range got {
			if s.Data["task_id"] == sub {
				out = append(out, s)
			}
		}
		return out
	}
	for deadline := time.Now().Add(20 * time.Second); len(ofSub()) < 1 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(700 * time.Millisecond)
	subSent := ofSub()
	if len(subSent) != 1 || subSent[0].Event != "task.created" || subSent[0].Delivery != subIDs[0] || subSent[0].EventID != subIDs[0] {
		t.Fatalf("the endpoint was sent %+v for the sub-task, want its one task.created, by its outbox event's id %s", subSent, subIDs[0])
	}
	if d := subSent[0].Data; d["parent_task_id"] != task || d["name"] != "Collect the changes" || d["project_id"] != w.project || d["created_by"] != ana {
		t.Errorf("the sub-task's task.created said %v, want its name, project, created_by ana and parent_task_id %s", d, task)
	}
}
