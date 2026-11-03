//go:build integration
// +build integration

package integration_test

// Sub-tasks made together (taskWriter.MakeSubTasks, CR-167 F1), against real
// Postgres and Dgraph, under seedWorkWorld's task that ana keeps to the
// project's admins: one batch makes every one under it, each kept to the same
// people before anyone is told of it, as the app's own sub-task is. Each
// hangs under the task, which says so in its history, and each one's
// task.created entry is the log's restricted one (CR-169 F5).
//
// Run: PKGS=./tests/integration/ (go test -tags=integration ./tests/integration/ -run '^TestSubTasksAreMadeTogetherUnderARestrictedTask$' -v)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskChangeModels "github.com/akashc777/OneCamp/models/postgres/TaskChange"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSubTasksAreMadeTogetherUnderARestrictedTask(t *testing.T) {
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

	asAna, ana := w.signedIn("ana")
	task, err := taskBusiness.GetDgraphBasicTaskInfo(asAna, w.task, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := workAccess.Restrict(asAna, task, ana.UserPostgresInfo.Id, nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	// The log's head before the batch: its entries are the ones after it.
	head, _, err := taskChangeModels.HeadAndOldest(asAna)
	if err != nil {
		t.Fatal(err)
	}
	under := &dgraphStruct.DgraphTask{Uuid: w.task, Project: &dgraphStruct.DgraphProject{Uuid: w.project}}
	names := []string{"Reproduce", "Fix", "Verify"}
	ids, err := taskWriter.MakeSubTasks(asAna, &ana, under, names)
	if err != nil || len(ids) != 3 {
		t.Fatalf("made %v: %v, want three", ids, err)
	}
	parent := uuid.MustParse(w.task)
	for i, id := range ids {
		r, err := restrictionModel.Of(asAna, id)
		if err != nil || !r.Restricted || r.Via != parent {
			t.Errorf("sub-task %d: %+v (%v), want kept to the people of %s", i, r, err, parent)
		}
	}
	// Each hangs under the task, by name, and the task's history adds each
	// once, as CreateSubTask's own node does (newTaskNodes' Under branch).
	got := readUnder(t, w.task)
	for i, id := range ids {
		if s, ok := got.sub[id.String()]; !ok || s.name != names[i] || s.parent != w.task {
			t.Errorf("sub-task %d (%s) read under the task: %+v (found %v), want %q whose parent is %s", i, id, s, ok, names[i], w.task)
		}
	}
	for _, n := range names {
		if got.added[n] != 1 {
			t.Errorf("the task's history adds %q %d times, want once (%v)", n, got.added[n], got.added)
		}
	}
	// Each one's task.created entry is the log's restricted one, read with its
	// batch, so its live notice and its webhook keep it to the same people.
	kept, err := taskChangeModels.After(asAna, head, 1000)
	if err != nil {
		t.Fatal(err)
	}
	marked := map[string]bool{}
	for _, k := range kept {
		if k.Kind == workFeed.KindCreated {
			marked[k.ResourceUUID] = k.Restricted
		}
	}
	for i, id := range ids {
		if restricted, logged := marked[id.String()]; !logged || !restricted {
			t.Errorf("sub-task %d's task.created entry: logged %v, restricted %v; want a restricted entry", i, logged, restricted)
		}
	}
	// ben, a member the task is kept from, can't make one under it.
	asBen, ben := w.signedIn("ben")
	if made, err := taskWriter.MakeSubTasks(asBen, &ben, under, []string{"Sneak"}); err == nil || len(made) != 0 {
		t.Errorf("ben made %v under a task kept from him: %v", made, err)
	}
}

// underRead is a task's sub-tasks as Dgraph keeps them (each one's name and
// the task it hangs under) and how many times its history adds each name.
type underRead struct {
	sub   map[string]struct{ name, parent string }
	added map[string]int
}

func readUnder(t *testing.T, taskID string) underRead {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(context.Background(),
		`query q($id: string) { q(func: eq(task_uuid, $id)) {
			task_sub_tasks { task_uuid task_name task_parent_task { task_uuid } }
			task_activities { activity_type activity_next_state } } }`,
		map[string]string{"$id": taskID})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Q []struct {
			Sub []struct {
				UUID   string `json:"task_uuid"`
				Name   string `json:"task_name"`
				Parent *struct {
					UUID string `json:"task_uuid"`
				} `json:"task_parent_task"`
			} `json:"task_sub_tasks"`
			Activity []struct {
				Type string `json:"activity_type"`
				Next string `json:"activity_next_state"`
			} `json:"task_activities"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.Q) != 1 {
		t.Fatalf("read task %s: %v (%d nodes)", taskID, err, len(out.Q))
	}
	r := underRead{sub: map[string]struct{ name, parent string }{}, added: map[string]int{}}
	for _, s := range out.Q[0].Sub {
		p := ""
		if s.Parent != nil {
			p = s.Parent.UUID
		}
		r.sub[s.UUID] = struct{ name, parent string }{s.Name, p}
	}
	for _, a := range out.Q[0].Activity {
		if a.Type == dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK {
			r.added[a.Next]++
		}
	}
	return r
}
