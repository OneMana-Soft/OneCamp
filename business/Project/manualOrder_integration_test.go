//go:build integration

package business

// L-1: the list's Manual sort reads the project's one order, page by page,
// against Dgraph with the production schema.
// Run: PKGS=./business/Project integ_rel.sh <wt> <log> 'TestTheListsManualSort'

import (
	"context"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestTheListsManualSort(t *testing.T) {
	ctx := context.Background()
	// The list read merges each task's GitHub facts from Postgres.
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	project := uuid.NewString()
	live := "0001-01-01T00:00:00Z"
	// a (ranked 10), b (never moved, newest), c (ranked 5), d (never moved, older), e done (ranked 1).
	rows := []struct {
		name, status, created string
		rank                  any
	}{
		{"a", "todo", "2026-10-01T00:00:00Z", 10.0},
		{"b", "todo", "2026-10-03T00:00:00Z", nil},
		{"c", "inProgress", "2026-10-01T00:00:00Z", 5.0},
		{"d", "todo", "2026-10-02T00:00:00Z", nil},
		{"e", "done", "2026-10-01T00:00:00Z", 1.0},
	}
	var nodes []map[string]any
	var edges []map[string]any
	for _, r := range rows {
		n := map[string]any{"uid": "_:" + r.name, "dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": r.name,
			"task_status": r.status, "task_deleted_at": live, "task_created_at": r.created}
		if r.rank != nil {
			n["task_rank"] = r.rank
		}
		nodes = append(nodes, n)
		edges = append(edges, map[string]any{"uid": "_:" + r.name})
	}
	dg.Mutate(t, append(nodes, map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "P",
		"project_deleted_at": live, "project_tasks": edges}))

	names := func(pageSize, pageIndex int, all bool, filter string) (string, uint64) {
		p, err := GetDgraphProjectTaskList(ctx, project, "0x1", filter, dgraphquery.ManualOrder, pageSize, pageIndex, all)
		if err != nil || p == nil {
			t.Fatalf("read: %v", err)
		}
		var out []string
		for _, x := range p.Tasks {
			out = append(out, x.Name)
		}
		return strings.Join(out, ","), p.TaskCount
	}
	if got, n := names(2, 0, false, ""); got != "b,d" || n != 5 {
		t.Fatalf("page 1 is %s of %d", got, n)
	}
	if got, _ := names(2, 1, false, ""); got != "e,c" {
		t.Fatalf("page 2 is %s", got)
	}
	if got, _ := names(0, 0, true, ""); got != "b,d,e,c,a" {
		t.Fatalf("all of it is %s", got)
	}
	if got, n := names(10, 0, false, `eq(task_status, "todo")`); got != "b,d,a" || n != 3 {
		t.Fatalf("To do is %s of %d", got, n)
	}
	if got, _ := names(10, 3, false, ""); got != "" {
		t.Fatalf("past the end is %q", got)
	}
}
