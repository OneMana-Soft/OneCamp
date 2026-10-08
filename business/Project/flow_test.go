package business

import (
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestFlowOfWork(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }
	at := func(d int) *time.Time { x := day(d); return &x }
	change := func(d int, from, to string) *dgraphStruct.DgraphTaskActivity {
		return &dgraphStruct.DgraphTaskActivity{Type: dgraphStruct.ACTIVITY_TYPE_STATUS, LogTime: at(d), PrevState: from, NextState: to}
	}
	task := func(made int, status string, changes ...*dgraphStruct.DgraphTaskActivity) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{CreatedAt: at(made), Status: status, Activity: changes}
	}
	first := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC) // a Monday
	now := day(25)
	projects := []domain.ReportProject{{
		UUID: "p",
		Open: []*dgraphStruct.DgraphTask{
			// Made in week 1, still to do.
			task(8, dgraphStruct.TASK_STATUS_TODO),
			// To do, then in progress in week 2, then in the project's own "QA"
			// (in review) in week 3.
			task(1, dgraphStruct.TASK_STATUS_INREVIEW,
				change(15, dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_INPROGRESS),
				change(22, dgraphStruct.TASK_STATUS_INPROGRESS, "QA")),
			// Made after the last week began, in the backlog: to do.
			task(23, dgraphStruct.TASK_STATUS_BACKLOG),
		},
		Closed: []*dgraphStruct.DgraphTask{
			// Done in week 2.
			task(2, dgraphStruct.TASK_STATUS_DONE, change(16, dgraphStruct.TASK_STATUS_INPROGRESS, dgraphStruct.TASK_STATUS_DONE)),
			// Done before the report began, reopened in week 1, done again in week 3.
			task(1, dgraphStruct.TASK_STATUS_DONE,
				change(3, dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_DONE),
				change(9, dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_INPROGRESS),
				change(23, dgraphStruct.TASK_STATUS_INPROGRESS, dgraphStruct.TASK_STATUS_DONE)),
			// Cancelled in week 2: it leaves to do and counts nowhere after.
			task(4, dgraphStruct.TASK_STATUS_CANCELED, change(17, dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_CANCELED)),
			// Renamed status since: "Polishing" is no status now; it reads as
			// the one before it, and the last change as its status now.
			task(2, dgraphStruct.TASK_STATUS_DONE,
				change(10, dgraphStruct.TASK_STATUS_TODO, "Polishing"),
				change(24, "Polishing", "Shipped")),
		},
	}}
	// Imported done on the 18th, with no history: to do until then.
	imported := task(2, dgraphStruct.TASK_STATUS_DONE)
	imported.StatusSince = at(18)
	projects[0].Closed = append(projects[0].Closed, imported)
	own := map[string]map[string]string{"p": {"qa": dgraphStruct.TASK_STATUS_INREVIEW}}
	flow := buildFlow(projects, own, first, now, 3, nil)
	want := []FlowWeek{
		// Week 1 (7–14 Sep): A, B, cancelled, polishing and the import to do; reopened in progress; D in progress.
		{ToDo: 5, InProgress: 2},
		// Week 2 (14–21 Sep): B in progress since the 15th, D done on the 16th, the import on the 18th, cancelled gone.
		{ToDo: 2, InProgress: 2, Done: 2},
		// Week 3 (to now, the 25th): B in review, reopened done again, polishing shipped, the new one to do.
		{ToDo: 2, InReview: 1, Done: 4},
	}
	for i := range want {
		if flow[i] != want[i] {
			t.Fatalf("week %d: %+v, want %+v (all: %+v)", i+1, flow[i], want[i], flow)
		}
	}
	if got := buildFlow(projects, own, first, now, 3, map[string]bool{"other": true}); got[2] != (FlowWeek{}) {
		t.Fatalf("narrowed to another project, nothing counts: %+v", got)
	}
}
