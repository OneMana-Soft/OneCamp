package Domain

import (
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// An epic's tasks are top-level work (jira-01 J-H): a plan's roots are what
// the top-level clause (TopLevel) admits, and its dependency hops take any
// live task, since a sub-task has no dependency edge (the hierarchy refuses
// one, and a task with dependencies can't become a sub-task).
func TestAPlansRootsAreTopLevelWorkAndItsHopsAnyLiveTask(t *testing.T) {
	const top = "(TOP)"
	for name, q := range map[string]string{
		"a plan read":      planBlocks("0", PlanLimits{Open: 5, Finished: 5}, top),
		"an unlimited one": planBlocks("1", PlanLimits{}, top),
		"a totals read":    totalsBlocks("p0", 2, top),
	} {
		if !strings.Contains(q, "project_tasks @filter("+dgraphStruct.TASK_LIVE_FILTER+" AND "+top) {
			t.Errorf("%s: its roots aren't the top-level clause", name)
		}
		if strings.Contains(q, "NOT has(task_parent_task)") {
			t.Errorf("%s: still reads one level", name)
		}
	}
	if !strings.Contains(planBlocks("0", PlanLimits{}, top), "task_blocked_by @facets(kind, lag) @filter("+dgraphStruct.TASK_LIVE_FILTER+")") {
		t.Errorf("a plan's hop isn't any live task")
	}
	if strings.Contains(downstreamQuery, "NOT has(task_parent_task)") {
		t.Errorf("moving tasks along skips an epic's tasks")
	}
}
