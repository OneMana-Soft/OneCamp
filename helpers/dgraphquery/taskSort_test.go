package dgraphquery

import (
	"os"
	"regexp"
	"testing"
)

// Each sort a task list offers, both ways, is the graph's order by its own
// predicate, Created by either of its keys; a key the list doesn't offer never
// reaches the graph.
func TestTaskListOrder(t *testing.T) {
	const newestFirst = "orderdesc: task_created_at"
	for in, want := range map[string]string{
		``:   newestFirst,
		`[]`: newestFirst,
		`[{"id":"task_start_date","desc":false}]`:   "orderasc: task_start_date",
		`[{"id":"task_start_date","desc":true}]`:    "orderdesc: task_start_date",
		`[{"id":"task_due_date","desc":false}]`:     "orderasc: task_due_date",
		`[{"id":"task_due_date","desc":true}]`:      "orderdesc: task_due_date",
		`[{"id":"task_created_at","desc":false}]`:   "orderasc: task_created_at",
		`[{"id":"task_created_at","desc":true}]`:    "orderdesc: task_created_at",
		`[{"id":"task_created_date","desc":false}]`: "orderasc: task_created_at",
		`[{"id":"task_created_date","desc":true}]`:  "orderdesc: task_created_at",
		// Not offered: passed over, never handed to the graph.
		`[{"id":"task_name","desc":false}]`:          newestFirst,
		`[{"id":"task_rank","desc":false}]`:          newestFirst,
		`[{"id":"uid","desc":true}]`:                 newestFirst,
		`[{"id":"task_due_date) { x","desc":false}]`: newestFirst,
		`[{"id":"TASK_DUE_DATE","desc":false}]`:      newestFirst,
		`[{"id":"task_due_date","desc":"yes"}]`:      newestFirst,
		`[{"id":"task_due_date"}]`:                   newestFirst,
		`[{"id":"task_due_date","desc":false},{"id":"task_name","desc":true},{"id":"task_start_date","desc":true}]`: "orderdesc: task_start_date",
	} {
		if got, err := TaskListOrder(in); err != nil || got != want {
			t.Errorf("TaskListOrder(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`not json`, `{"id":"task_due_date"}`, `[1]`} {
		if _, err := TaskListOrder(bad); err == nil {
			t.Errorf("TaskListOrder(%s) took a parameter that isn't a list of sorts", bad)
		}
	}
}

// Every predicate a list sorts by is one the graph's schema declares: the
// graph refuses a sort by any other ("Cannot sort by unknown attribute"), and
// the list with it.
func TestTaskSortsArePredicatesTheSchemaDeclares(t *testing.T) {
	schema, err := os.ReadFile("../../initializers/dgraphInit/connectDgraphDB.go")
	if err != nil {
		t.Fatal(err)
	}
	for key, predicate := range taskSorts {
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(predicate) + `:\s*\w+`).Match(schema) {
			t.Errorf("%s sorts by %s, which the schema doesn't declare", key, predicate)
		}
	}
}
