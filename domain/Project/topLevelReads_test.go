package Domain

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/models/taskread"
)

// The backlog and the timeline read top-level work (jira-01 J-H): a task at
// the top or in an epic, by domain/Task TopLevel's block and clause, never a
// task's sub-tasks.
func TestTheBacklogAndTheTimelineReadAnEpicsTasksAsTopLevelWork(t *testing.T) {
	const block, top = "var(BLOCK)", "(TOP)"
	for name, q := range map[string]string{
		"the backlog":  backlogQuery(taskread.Filter{}, block, top, nil, nil),
		"the timeline": timelineQuery(taskread.Filter{}, block, top, "", 50),
	} {
		if !strings.Contains(q, block) {
			t.Errorf("%s: no top-level block", name)
		}
		if !strings.Contains(q, "AND "+top) {
			t.Errorf("%s: its tasks aren't the top-level clause", name)
		}
		if strings.Contains(q, "NOT has(task_parent_task)") {
			t.Errorf("%s: still reads one level", name)
		}
	}
	if q := timelineQuery(taskread.Filter{}, block, top, "", 50); strings.Count(q, "AND "+top) != 2 {
		t.Errorf("the timeline's count and list read different tasks")
	}
}
