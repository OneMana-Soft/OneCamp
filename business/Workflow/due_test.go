package business

import (
	"context"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// A due trigger fires at its time of day in its zone: due soon on the day
// `days` before the due day, overdue on the day after it.
func TestDueTriggersFireOnTheirDayAtTheirTime(t *testing.T) {
	kolkata := zone(t, "Asia/Kolkata")
	// The web writes a due date as the midnight its day starts.
	due := time.Date(2026, 10, 22, 0, 0, 0, 0, kolkata)
	soon, err := checkDue(workflowModel.TriggerTaskDueSoon, dueConfig{Days: 1, At: "09:00", TZ: "Asia/Kolkata"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := soon.fireAt(workflowModel.TriggerTaskDueSoon, due), time.Date(2026, 10, 21, 9, 0, 0, 0, kolkata); !got.Equal(want) {
		t.Errorf("due soon: %s, want %s", got, want)
	}
	late, err := checkDue(workflowModel.TriggerTaskOverdue, dueConfig{Days: 3, TZ: "Asia/Kolkata"})
	if err != nil {
		t.Fatal(err)
	}
	if late.cfg.Days != 0 || late.cfg.At != "09:00" {
		t.Errorf("overdue keeps no days and defaults to 09:00: %+v", late.cfg)
	}
	if got, want := late.fireAt(workflowModel.TriggerTaskOverdue, due), time.Date(2026, 10, 23, 9, 0, 0, 0, kolkata); !got.Equal(want) {
		t.Errorf("overdue: %s, want the day after the due day, %s", got, want)
	}
	for _, bad := range []dueConfig{{Days: -1}, {Days: 31}, {At: "nine"}, {TZ: "Nowhere/Else"}} {
		if _, err := checkDue(workflowModel.TriggerTaskDueSoon, bad); err == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
}

// dueHarness puts rules in the registry and tasks in the project the sweep
// reads.
type dueHarness struct {
	tasks []*dgraphStruct.DgraphTask
}

func newDueHarness(t *testing.T, f *fakeStore, rules ...*compiledWorkflow) *dueHarness {
	t.Helper()
	h := &dueHarness{}
	oldReg, oldOpen, oldClaimed := byTrigger, openTasks, claimedKeys
	t.Cleanup(func() {
		regMu.Lock()
		byTrigger = oldReg
		regMu.Unlock()
		openTasks, claimedKeys = oldOpen, oldClaimed
	})
	reg := map[string][]*compiledWorkflow{}
	for _, r := range rules {
		reg[r.triggerType] = append(reg[r.triggerType], r)
	}
	regMu.Lock()
	byTrigger = reg
	regMu.Unlock()
	openTasks = func(_ context.Context, projects []string, from, to *time.Time, _ int) (map[string][]*dgraphStruct.DgraphTask, error) {
		out := map[string][]*dgraphStruct.DgraphTask{}
		for _, project := range projects {
			for _, task := range h.tasks {
				if task.Project.Uuid == project && (from == nil || !task.DueDate.Before(*from)) && (to == nil || !task.DueDate.After(*to)) {
					out[project] = append(out[project], task)
				}
			}
		}
		return out, nil
	}
	claimedKeys = func(_ context.Context, _ uuid.UUID, keys []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, k := range keys {
			if _, ok := f.runs[k]; ok {
				out[k] = true
			}
		}
		return out, nil
	}
	return h
}

func dueRule(trigger, project string, days int, then []WorkflowAction) *compiledWorkflow {
	d, _ := checkDue(trigger, dueConfig{Days: days, At: "09:00"})
	return &compiledWorkflow{id: uuid.New(), name: "Nudge", createdBy: "owner", triggerType: trigger, projectID: project,
		due: d, loc: d.loc, actions: then}
}

// Each due date fires a rule once, and a date moved later fires it again.
func TestEachDueDateFiresOnce(t *testing.T) {
	f := newStore(t)
	pid := f.task.Project.Uuid
	r := dueRule(workflowModel.TriggerTaskOverdue, pid, 0, []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}})
	h := newDueHarness(t, f, r)
	due := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	f.task.DueDate = &due
	h.tasks = []*dgraphStruct.DgraphTask{f.task}
	clock = func() time.Time { return time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC) }
	t.Cleanup(func() { clock = time.Now })

	for i := 0; i < 3; i++ {
		if err := sweepDue(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.ops) != 1 || !strings.Contains(f.ops[0], "high") {
		t.Fatalf("ops %v, want the task raised once", f.ops)
	}
	if _, ok := f.runs[dueKey(workflowModel.TriggerTaskOverdue, f.task.Uuid, due, time.UTC)]; !ok {
		t.Error("no run keyed by the task and its due day")
	}

	moved := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	f.task.DueDate, f.task.Priority = &moved, "low"
	clock = func() time.Time { return time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC) }
	if err := sweepDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != 2 {
		t.Errorf("ops %v, want the moved date to fire again", f.ops)
	}
}

// Nothing fires before its time, nor long after it: a new rule doesn't wake
// every task that went overdue last month.
func TestDueTriggersKeepToTheirWindow(t *testing.T) {
	f := newStore(t)
	pid := f.task.Project.Uuid
	r := dueRule(workflowModel.TriggerTaskDueSoon, pid, 1, []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}})
	h := newDueHarness(t, f, r)
	due := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	f.task.DueDate = &due
	h.tasks = []*dgraphStruct.DgraphTask{f.task}
	t.Cleanup(func() { clock = time.Now })

	clock = func() time.Time { return time.Date(2026, 10, 21, 8, 59, 0, 0, time.UTC) }
	_ = sweepDue(context.Background())
	if len(f.ops) != 0 {
		t.Fatalf("fired at 08:59 for 09:00: %v", f.ops)
	}
	clock = func() time.Time { return time.Date(2026, 10, 22, 9, 1, 0, 0, time.UTC) }
	_ = sweepDue(context.Background())
	if len(f.ops) != 0 {
		t.Fatalf("fired more than a day late: %v", f.ops)
	}
	clock = func() time.Time { return time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC) }
	_ = sweepDue(context.Background())
	if len(f.ops) != 1 {
		t.Fatalf("ops %v, want it fired the evening before", f.ops)
	}
}

// A pass takes at most duePerRule fires of one rule; the rest wait for the
// next pass, which keeps a busy day under the limits that stop runaway rules.
func TestABusyDayIsSpreadOverPasses(t *testing.T) {
	f := newStore(t)
	pid := f.task.Project.Uuid
	r := dueRule(workflowModel.TriggerTaskOverdue, pid, 0, []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}})
	h := newDueHarness(t, f, r)
	due := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for i := 0; i < duePerRule+7; i++ {
		h.tasks = append(h.tasks, &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Name: "Task", Status: "todo", Priority: "low",
			DueDate: &due, Project: &dgraphStruct.DgraphProject{Uuid: pid}})
	}
	clock = func() time.Time { return time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC) }
	t.Cleanup(func() { clock = time.Now })
	_ = sweepDue(context.Background())
	if len(f.runs) != duePerRule {
		t.Fatalf("first pass ran %d, want %d", len(f.runs), duePerRule)
	}
	_ = sweepDue(context.Background())
	if len(f.runs) != duePerRule+7 {
		t.Errorf("second pass left %d of %d", duePerRule+7-len(f.runs), duePerRule+7)
	}
}
