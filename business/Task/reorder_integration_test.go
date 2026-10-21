//go:build integration

package business

// L-1: one order per project, against Dgraph with the production schema. A
// task dragged in the list between tasks of another status keeps its place
// in its own board column too; a crowded gap renumbers the whole project, so
// every column and the list still agree.
// Run: PKGS=./business/Task integ_rel.sh <wt> <log> 'TestOneOrderEverywhere'

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestOneOrderEverywhere(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	old := reorderSeams.publish
	t.Cleanup(func() { reorderSeams.publish = old })
	var told *mqttStruct.MqttTaskChanged
	reorderSeams.publish = func(m *mqttStruct.MqttTaskChanged) { told = m }

	project := uuid.NewString()
	live := "0001-01-01T00:00:00Z"
	// The list's order: t1 (todo), p1 (in progress), t2 (todo), p2 (in progress).
	type row struct {
		name, status string
		rank         float64
	}
	rows := []row{{"t1", "todo", 0}, {"p1", "inProgress", 1024}, {"t2", "todo", 2048}, {"p2", "inProgress", 3072}}
	id := map[string]string{}
	var tasks []map[string]any
	for _, r := range rows {
		id[r.name] = uuid.NewString()
		tasks = append(tasks, map[string]any{"uid": "_:" + r.name, "dgraph.type": "Task", "task_uuid": id[r.name], "task_name": r.name,
			"task_status": r.status, "task_rank": r.rank, "task_deleted_at": live, "task_created_at": "2026-10-01T00:00:00Z"})
	}
	project0 := map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "P", "project_deleted_at": live, "project_tasks": []map[string]any{}}
	for _, r := range rows {
		project0["project_tasks"] = append(project0["project_tasks"].([]map[string]any), map[string]any{"uid": "_:" + r.name})
	}
	dg.Mutate(t, append(tasks, project0))

	order := func(status string) string {
		var out struct {
			P []struct {
				T []*dgraphStruct.DgraphTask `json:"t"`
			} `json:"p"`
		}
		filter := ""
		if status != "" {
			filter = ` @filter(eq(task_status, "` + status + `"))`
		}
		_ = json.Unmarshal(dg.Query(t, `{ p(func: eq(project_uuid, "`+project+`")) { t: project_tasks`+filter+` { task_uuid task_name task_rank task_created_at } } }`), &out)
		ts := out.P[0].T
		taskrank.Sort(ts)
		var names []string
		for _, x := range ts {
			names = append(names, x.Name)
		}
		return strings.Join(names, ",")
	}

	// In the list, t2 goes to the top, above t1: in the To do column too.
	if err := Reorder(ctx, project, []*dgraphStruct.DgraphTask{{Uuid: id["t2"]}}, "", id["t1"]); err != nil {
		t.Fatal(err)
	}
	if got := order(""); got != "t2,t1,p1,p2" {
		t.Fatalf("the list is %s", got)
	}
	if got := order("todo"); got != "t2,t1" {
		t.Fatalf("To do is %s", got)
	}
	if told == nil || told.ProjectUuid != project || len(told.Ranks) != 1 {
		t.Fatalf("told %+v", told)
	}

	// A selection, p2 then t1, dropped between t2 and p1, keeps its order.
	if err := Reorder(ctx, project, []*dgraphStruct.DgraphTask{{Uuid: id["p2"]}, {Uuid: id["t1"]}}, id["t2"], id["p1"]); err != nil {
		t.Fatal(err)
	}
	if got := order(""); got != "t2,p2,t1,p1" {
		t.Fatalf("after the selection, the list is %s", got)
	}

	// Crowd the gap after t2 until there's no room (each move halves it): the
	// project is renumbered, and the order holds in the list and each column.
	renumbered := 0
	for i := 0; i < 60; i++ {
		if err := Reorder(ctx, project, []*dgraphStruct.DgraphTask{{Uuid: id["p1"]}}, id["t2"], id["p2"]); err != nil {
			t.Fatal(err)
		}
		renumbered = max(renumbered, len(told.Ranks))
		if err := Reorder(ctx, project, []*dgraphStruct.DgraphTask{{Uuid: id["p2"]}}, id["t2"], id["p1"]); err != nil {
			t.Fatal(err)
		}
		renumbered = max(renumbered, len(told.Ranks))
	}
	if got := order(""); got != "t2,p2,p1,t1" {
		t.Fatalf("after crowding, the list is %s", got)
	}
	if got := order("inProgress"); got != "p2,p1" {
		t.Fatalf("In progress is %s", got)
	}
	if renumbered != 4 {
		t.Fatalf("a crowded gap renumbered %d tasks, want the project's 4", renumbered)
	}
}
