package business

import (
	"context"
	"strings"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// linesNamingATask are the history lines that hold another task's uuid, in
// NextState or PrevState (a state that isn't a uuid, such as a label, is
// words, not a task).
var linesNamingATask = map[string]bool{
	dgraphStruct.ACTIVITY_TYPE_ADD_DEPENDENCY:    true,
	dgraphStruct.ACTIVITY_TYPE_REMOVE_DEPENDENCY: true,
	dgraphStruct.ACTIVITY_TYPE_ADD_LINK:          true,
	dgraphStruct.ACTIVITY_TYPE_REMOVE_LINK:       true,
	dgraphStruct.ACTIVITY_TYPE_PARENT:            true,
	dgraphStruct.ACTIVITY_TYPE_REMOVE_SUB_TASK:   true,
}

// keepHistorySeen takes out of the task's history lines every other task the
// reader (ctx's, by graph uid userUID) may not see now, by Work Access's See:
// restricted from them, deleted, in a project they aren't in, or gone. A line
// that holds such a task's uuid loses it; a sub-task line, which names its
// sub-task by name only (the name it had then), loses it once any task the
// lines may name is hidden, unless a seen task carries it. The line stays:
// what happened, never which task. One read of the tasks the lines name
// (with the task's sub-tasks when a sub-task line is there) and one JudgeTasks
// for the whole page. Background work reads everything.
func keepHistorySeen(ctx context.Context, userUID string, task *dgraphStruct.DgraphTask) error {
	if task == nil || len(task.Activity) == 0 || helpers.IsSystemRead(ctx) {
		return nil
	}
	var ids []string
	subLines := false
	for _, l := range task.Activity {
		switch {
		case l == nil:
		case linesNamingATask[l.Type]:
			for _, s := range []string{l.NextState, l.PrevState} {
				if _, err := uuid.Parse(s); err == nil {
					ids = append(ids, s)
				}
			}
		case l.Type == dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK && l.NextState != "":
			subLines = true
		}
	}
	if len(ids) == 0 && !subLines {
		return nil
	}
	ofTask := ""
	if subLines {
		ofTask = task.Uuid
	}
	named, err := domain.GetHistoryTasksFor(ctx, ids, ofTask, userUID)
	if err != nil {
		return err
	}
	verdicts, err := workAccess.JudgeTasks(ctx, taskread.ReaderOf(ctx), userUID, named...)
	if err != nil {
		return err
	}
	blankUnseenLines(task.Activity, named, verdicts)
	return nil
}

// blankUnseenLines is keepHistorySeen's decision, given the tasks the lines
// may name (read whole) and the reader's verdict on each: a uuid in a line
// stays only when its task is seen (other words stay). A sub-task line names
// the sub-task as it was called then, and a hidden one may have been renamed
// since, so once any of the tasks is one they may not see, a sub-task line's
// name stays only when a task they see carries it (fail closed). Pure.
func blankUnseenLines(lines []*dgraphStruct.DgraphTaskActivity, named []*dgraphStruct.DgraphTask, verdicts map[string]workAccess.Verdict) {
	hidden, seenName := false, map[string]bool{}
	for _, t := range named {
		switch {
		case t == nil:
		case verdicts[strings.ToLower(t.Uuid)].See:
			seenName[t.Name] = true
		default:
			hidden = true
		}
	}
	seen := func(s string) string {
		if _, err := uuid.Parse(s); err != nil || verdicts[strings.ToLower(s)].See {
			return s
		}
		return ""
	}
	for _, l := range lines {
		switch {
		case l == nil:
		case linesNamingATask[l.Type]:
			l.NextState, l.PrevState = seen(l.NextState), seen(l.PrevState)
		case l.Type == dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK && hidden && !seenName[l.NextState]:
			l.NextState = ""
		}
	}
}
