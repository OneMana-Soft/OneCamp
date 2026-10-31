package business

import (
	"context"
	"strings"
	"testing"
	"time"

	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// A schedule for each task is one run: it acts on the open tasks its
// conditions hold for, as its owner, keeps each step with its task, and says
// how many matched.
func TestAScheduleForEachTaskIsOneRunOverTheTasksThatMatch(t *testing.T) {
	f := newStore(t)
	pid := f.task.Project.Uuid
	high := func(name string, prio string) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Name: name, Status: "todo", Priority: prio,
			Project: &dgraphStruct.DgraphProject{Uuid: pid}, Assignee: &dgraphStruct.DgraphUser{Uuid: "maya"}}
	}
	tasks := []*dgraphStruct.DgraphTask{high("Ship login", "high"), high("Fix footer", "low"), high("Rotate keys", "high")}
	oldOpen := openTasks
	t.Cleanup(func() { openTasks = oldOpen })
	openTasks = func(_ context.Context, projects []string, _, _ *time.Time, _ int) (map[string][]*dgraphStruct.DgraphTask, error) {
		if len(projects) != 1 || projects[0] != pid {
			t.Errorf("read projects %v, want the rule's", projects)
		}
		return map[string][]*dgraphStruct.DgraphTask{pid: tasks}, nil
	}
	cw := &compiledWorkflow{id: uuid.New(), name: "Friday nudge", createdBy: "owner", triggerType: workflowModel.TriggerSchedule,
		projectID: pid, loc: time.UTC,
		conditions: Conditions{Match: "all", Items: []Condition{{Field: "priority", Op: "is", Value: "high"}}},
		actions:    []WorkflowAction{{Type: ActionNotify, Notify: NotifyAssignee, Text: "{task} is still open"}}}
	now := time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)
	ev := triggerEvent{projectID: pid, eventKey: scheduleKey(now), text: cw.name}
	cw.runEachTask(context.Background(), ev, now)
	cw.runEachTask(context.Background(), ev, now)

	run := f.runs[scheduleKey(now)]
	if run == nil || run.Matched == nil || *run.Matched != 2 {
		t.Fatalf("run %+v, want one run that matched 2", run)
	}
	if len(run.Steps) != 2 || run.Steps[0].TaskID != tasks[0].Uuid || run.Steps[1].TaskID != tasks[2].Uuid {
		t.Errorf("steps %+v, want one per matching task, each with its task", run.Steps)
	}
	if len(f.told) != 2 || !strings.Contains(f.told[0], "Ship login is still open") || !strings.Contains(f.told[1], "Rotate keys is still open") {
		t.Errorf("told %v, want each task's own words, once", f.told)
	}
	if run.Subject != "2 tasks" {
		t.Errorf("subject %q", run.Subject)
	}
}

// It acts on at most maxEachTask tasks a run; the history says how many
// more matched.
func TestAScheduleForEachTaskStopsAtItsBound(t *testing.T) {
	f := newStore(t)
	pid := f.task.Project.Uuid
	var tasks []*dgraphStruct.DgraphTask
	for i := 0; i < maxEachTask+5; i++ {
		tasks = append(tasks, &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Name: "T", Status: "todo", Priority: "low",
			Project: &dgraphStruct.DgraphProject{Uuid: pid}})
	}
	oldOpen := openTasks
	t.Cleanup(func() { openTasks = oldOpen })
	openTasks = func(context.Context, []string, *time.Time, *time.Time, int) (map[string][]*dgraphStruct.DgraphTask, error) {
		return map[string][]*dgraphStruct.DgraphTask{pid: tasks}, nil
	}
	cw := &compiledWorkflow{id: uuid.New(), name: "Raise", createdBy: "owner", triggerType: workflowModel.TriggerSchedule,
		projectID: pid, loc: time.UTC, actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}
	now := time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)
	cw.runEachTask(context.Background(), triggerEvent{projectID: pid, eventKey: scheduleKey(now)}, now)
	run := f.runs[scheduleKey(now)]
	if run == nil || *run.Matched != maxEachTask+5 || len(run.Steps) != maxEachTask {
		t.Fatalf("matched %v, steps %d; want %d matched and %d acted on", run.Matched, len(run.Steps), maxEachTask+5, maxEachTask)
	}
}

// An owner who left the project has the run skipped and said.
func TestAScheduleWhoseOwnerLeftIsSkipped(t *testing.T) {
	f := newStore(t)
	pid := f.task.Project.Uuid
	access.inProject = func(context.Context, string, string) (bool, error) { return false, nil }
	cw := &compiledWorkflow{id: uuid.New(), name: "Raise", createdBy: "owner", triggerType: workflowModel.TriggerSchedule,
		projectID: pid, loc: time.UTC, actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}
	now := time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)
	cw.runEachTask(context.Background(), triggerEvent{projectID: pid, eventKey: scheduleKey(now)}, now)
	run := f.runs[scheduleKey(now)]
	if run == nil || run.Status != workflowModel.RunSkipped || !strings.Contains(run.Error, "isn't in the project") {
		t.Fatalf("run %+v, want skipped because the owner left", run)
	}
}

// A schedule that makes a task acts on it with the steps after: assigned and
// given a due date, with the run's history about the task it made.
func TestAScheduleActsOnTheTaskItMakes(t *testing.T) {
	f := newStore(t)
	pid := f.task.Project.Uuid
	oldMake := makeTask
	t.Cleanup(func() { makeTask = oldMake })
	made := uuid.New()
	makeTask = func(_ context.Context, _ *userModels.UserInfo, project uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		if project.String() != pid || in.Name != "Weekly review" {
			t.Errorf("made %q in %s", in.Name, project)
		}
		f.task.Uuid, f.task.Name = made.String(), in.Name
		return made, nil
	}
	f.members = map[string]bool{"maya": false}
	cw := &compiledWorkflow{id: uuid.New(), name: "Monday review", createdBy: "owner", triggerType: workflowModel.TriggerSchedule,
		projectID: pid, loc: time.UTC, actions: []WorkflowAction{
			{Type: ActionCreateTask, ProjectID: pid, TaskName: "Weekly review"},
			{Type: ActionAssign, Assign: AssignPerson, UserIDs: []string{"maya"}},
			{Type: ActionSetDue, DueInDays: days(4)},
		}}
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = time.Now })
	runWorkflow(context.Background(), cw, triggerEvent{projectID: pid, eventKey: scheduleKey(now), text: cw.name})
	run := f.runs[scheduleKey(now)]
	if run == nil || run.Status != workflowModel.RunOK {
		t.Fatalf("run %+v", run)
	}
	if run.SubjectID != made.String() || run.Subject != "Weekly review" {
		t.Errorf("run is about %q %q, want the task it made", run.SubjectID, run.Subject)
	}
	if len(run.Steps) != 3 || run.Steps[1].Detail != "assigned" || !strings.HasPrefix(run.Steps[2].Detail, "due 30 Oct") {
		t.Errorf("steps %+v", run.Steps)
	}
}
