//go:build integration

package integration_test

// linear-39 through the handlers: every change the Work Change Log keeps to a
// task reaches webhooks as task.updated, with the fields it changed and their
// new values, through the Outbox (business/WorkFeed sinks.go), against real
// Postgres 12 with every migration:
//   - the entry and its event are written together: the event names the
//     entry by its number;
//   - the endpoint is sent the change once, signed, with the event's own id
//     as its delivery id, and the event is done;
//   - a made task writes no task.updated event (task.created is its own).
//
// Run: go test -tags=integration ./tests/integration/ -run TestEveryChangeToATaskReachesWebhooksThroughTheOutbox -v

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

func TestEveryChangeToATaskReachesWebhooksThroughTheOutbox(t *testing.T) {
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
		delivery string
		body     []byte
	}
	var mu sync.Mutex
	var got []sent
	endpoint := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, sent{r.Header.Get("X-OneCamp-Delivery"), b})
		mu.Unlock()
		rw.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(endpoint.Close)

	// Ana, an admin of the project, sets a webhook up for its task changes.
	hook := uuid.New()
	w.exec(`INSERT INTO webhooks (id, name, type, token, secret, target_url, events, is_active, created_by, bot_name, scope_type, scope_entity_id, created_at, updated_at)
		VALUES ($1, 'Changes', 'outgoing', $2, 'shh', $3, '["task.updated"]', true, $4, 'Changes', 'project', $5, NOW(), NOW())`,
		hook, uuid.NewString(), endpoint.URL, w.people["ana"], w.project)

	// One change through the app's own route.
	if code := pastCall(t, taskController.UpdateTaskPriority, http.MethodPost, "/",
		map[string]any{"task_uuid": w.task, "task_priority": "high", "task_project_uuid": w.project}, w.account("ana"), nil, nil); code != http.StatusOK {
		t.Fatalf("priority: %d", code)
	}
	var seq int64
	if err := w.env.PG.QueryRow(`SELECT sequence FROM task_changes WHERE resource_uuid = $1 AND kind = 'task.updated' ORDER BY sequence DESC LIMIT 1`, w.task).Scan(&seq); err != nil {
		t.Fatalf("the change isn't in the log: %v", err)
	}

	// Its event was written with it, naming the entry by its number.
	var eventID, state string
	if err := w.env.PG.QueryRow(`SELECT event_id::text, state FROM outbox WHERE topic = 'work.changes' AND kind = 'task.updated' AND entities->>'sequence' = $1`,
		fmt.Sprint(seq)).Scan(&eventID, &state); err != nil {
		t.Fatalf("entry %d has no event in the outbox: %v", seq, err)
	}

	// The endpoint is sent it once, with the event's id.
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("the endpoint was sent %d requests, want the change once", len(got))
	}
	if got[0].delivery != eventID {
		t.Errorf("delivery id %q, want the event's own %q", got[0].delivery, eventID)
	}
	var body struct {
		Event   string `json:"event"`
		EventID string `json:"event_id"`
		Data    struct {
			TaskID    string         `json:"task_id"`
			ProjectID string         `json:"project_id"`
			Sequence  int64          `json:"sequence"`
			Fields    []string       `json:"fields"`
			Changes   map[string]any `json:"changes"`
			By        string         `json:"updated_by_uuid"`
			Source    string         `json:"source"`
			At        string         `json:"changed_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatalf("body %s: %v", got[0].body, err)
	}
	d := body.Data
	if body.Event != "task.updated" || body.EventID != eventID || d.TaskID != w.task || d.ProjectID != w.project || d.Sequence != seq ||
		!reflect.DeepEqual(d.Fields, []string{"task_priority"}) || d.Changes["task_priority"] != "high" ||
		d.By != w.people["ana"].String() || d.Source != "app" || d.At == "" {
		t.Errorf("sent %s, want task.updated of %s in %s, entry %d, task_priority high, by ana from the app", got[0].body, w.task, w.project, seq)
	}

	// The event is done.
	for deadline := time.Now().Add(5 * time.Second); state != "done" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := w.env.PG.QueryRow(`SELECT state FROM outbox WHERE event_id = $1`, eventID).Scan(&state); err != nil {
			t.Fatal(err)
		}
	}
	if state != "done" {
		t.Errorf("the event is %q, want done", state)
	}

	// A made task's entry writes one task.created event of its own, never a
	// task.updated (linear-39 round 2: every task change through the one path).
	if code := pastCall(t, taskController.CreateTask, http.MethodPost, "/",
		map[string]any{"task_project_uuid": w.project, "task_name": "Release notes"}, w.account("ana"), nil, nil); code != http.StatusOK {
		t.Fatalf("create: %d", code)
	}
	var made string
	if err := w.env.PG.QueryRow(`SELECT resource_uuid::text FROM task_changes WHERE kind = 'task.created' AND project_uuid = $1 ORDER BY sequence DESC LIMIT 1`, w.project).Scan(&made); err != nil {
		t.Fatalf("the made task isn't in the log: %v", err)
	}
	var kinds []string
	rows, err := w.env.PG.Query(`SELECT kind FROM outbox WHERE topic = 'work.changes' AND key = $1 ORDER BY id`, "task:"+made)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	rows.Close()
	if !reflect.DeepEqual(kinds, []string{"task.created"}) {
		t.Errorf("a made task wrote the events %v, want one task.created", kinds)
	}
}
