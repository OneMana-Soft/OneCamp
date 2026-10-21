package dgraphquery

import "testing"

// L-1: the list's Manual sort is the project's one order (business/TaskRank),
// which the graph can't sort by (a task never moved has no rank): it is
// asked for as ManualOrder, and the caller orders the tasks itself.
func TestManualIsTheProjectsOrder(t *testing.T) {
	got, err := TaskListOrder(`[{"id":"task_rank","desc":false}]`)
	if err != nil || got != ManualOrder {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := TaskListOrder(`[{"id":"task_rank","desc":false},{"id":"task_due_date","desc":true}]`); got != "orderdesc: task_due_date" {
		t.Fatalf("the last sort still wins: %q", got)
	}
}
