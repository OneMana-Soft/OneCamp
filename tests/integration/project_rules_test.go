//go:build integration
// +build integration

package integration_test

// A project's rules end to end (package 9 R1, design/projects-vs-asana-jira
// /PROGRAM.md §16), against real Postgres 12 and Dgraph: an event on the bus
// runs the rule as its owner through the Task Writer, once, on the branch its
// conditions choose, with a run row; Undo puts back what the task still
// holds; an owner who can no longer see the task changes nothing.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAProjectRule -v

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
)

// runsOf waits for a rule's runs to settle at n and answers them.
func runsOf(t *testing.T, project uuid.UUID, rule uuid.UUID, n int) []*workflowModel.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		runs, err := workflowModel.ListRuns(context.Background(), &project, &rule, nil, 50)
		if err != nil {
			t.Fatal(err)
		}
		finished := 0
		for _, r := range runs {
			if r.Status != workflowModel.RunRunning {
				finished++
			}
		}
		if finished >= n || time.Now().After(deadline) {
			if finished != n {
				t.Fatalf("%d finished runs, want %d: %+v", finished, n, runs)
			}
			return runs
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestAProjectRuleRunsOnceAsItsOwnerAndCanBeUndone(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	workflowBusiness.Start(ctx)
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)

	due := 2
	rule, err := workflowBusiness.CreateProjectRule(ctx, &ana, project, workflowBusiness.WorkflowInput{
		Name: "Share new work out", IsActive: true, TriggerType: workflowModel.TriggerTaskCreated,
		Conditions: workflowBusiness.Conditions{Items: []workflowBusiness.Condition{{Field: "assignee", Op: "is_empty"}}},
		Actions: []workflowBusiness.WorkflowAction{
			{Type: workflowBusiness.ActionAssign, Assign: workflowBusiness.AssignRoundRobin, UserIDs: []string{w.people["ben"].String()}},
			{Type: workflowBusiness.ActionSetPriority, Priority: "high"},
			{Type: workflowBusiness.ActionSetDue, DueInDays: &due},
		},
	})
	if err != nil {
		t.Fatalf("an admin makes a rule: %v", err)
	}

	// Someone outside the project's people can't be named.
	if _, err := workflowBusiness.CreateProjectRule(ctx, &ana, project, workflowBusiness.WorkflowInput{
		Name: "x", TriggerType: workflowModel.TriggerTaskCreated,
		Actions: []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionAssign, Assign: workflowBusiness.AssignPerson, UserIDs: []string{w.people["eve"].String()}}},
	}); err == nil {
		t.Error("a rule named someone outside the project")
	}

	// The event as the bus carries it.
	created := map[string]interface{}{"task_id": w.task, "project_id": w.project, "name": "Write the launch post", "created_by": w.people["ana"].String()}
	webhookBusiness.DispatchEvent(ctx, "task.created", created)
	runs := runsOf(t, project, rule.Id, 1)
	run := runs[0]
	if run.Status != workflowModel.RunOK || run.Branch != "then" || len(run.Steps) != 3 {
		t.Fatalf("the run: %+v", run)
	}
	got, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee == nil || got.Assignee.Uuid != w.people["ben"].String() || got.Priority != "high" || got.DueDate == nil || got.DueDate.Year() < 2000 {
		t.Fatalf("the task after the rule: assignee %+v priority %q due %v", got.Assignee, got.Priority, got.DueDate)
	}
	// Its writes don't wake it (or any rule): it ran once, not again for its
	// own assignment and priority.
	time.Sleep(500 * time.Millisecond)
	runsOf(t, project, rule.Id, 1)

	// Now that someone has it, the condition fails and there is no otherwise:
	// nothing runs and nothing is recorded.
	webhookBusiness.DispatchEvent(ctx, "task.created", created)
	time.Sleep(time.Second)
	runsOf(t, project, rule.Id, 1)

	// Undo, as a member who may change the task: each change goes back.
	if _, err := workflowBusiness.UndoRun(ctx, &ben, project, run.ID); err != nil {
		t.Fatalf("undo: %v", err)
	}
	got, _ = taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if got.Assignee != nil && got.Assignee.Uuid != "" {
		t.Errorf("undone, it's still assigned to %s", got.Assignee.Uuid)
	}
	if got.DueDate != nil && got.DueDate.Year() > 1970 {
		t.Errorf("undone, it's still due %v", got.DueDate)
	}
	again, _ := workflowModel.GetRun(ctx, run.ID)
	if again.Status != workflowModel.RunUndone || again.UndoneBy == nil || *again.UndoneBy != ben.UserPostgresInfo.Id {
		t.Errorf("the run after undo: %+v", again)
	}
	if _, err := workflowBusiness.UndoRun(ctx, &ben, project, run.ID); err == nil {
		t.Error("a run was undone twice")
	}

	// A rule whose owner is no longer in the project changes nothing. cy (in
	// the team, not the project) turns it on and so becomes its owner; the
	// engine reloads, as every change through the business layer does.
	cy := w.account("cy")
	if err := workflowBusiness.SetProjectRuleActive(ctx, &cy, project, rule.Id, true); err != nil {
		t.Fatal(err)
	}
	webhookBusiness.DispatchEvent(ctx, "task.created", created)
	runs = runsOf(t, project, rule.Id, 2)
	if runs[0].Status != workflowModel.RunSkipped {
		t.Errorf("an owner outside the project: %+v", runs[0])
	}
	got, _ = taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if got.Assignee != nil && got.Assignee.Uuid != "" {
		t.Error("a rule acted for an owner outside the project")
	}
}

// The run store: one run per event key, turns that never repeat under
// contention, one undo, and old runs purged.
func TestARulesRunsAreClaimedOnceAndKeptNinetyDays(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	id, err := workflowModel.CreateWorkflow(ctx, ana.UserPostgresInfo.Id, workflowModel.Fields{Name: "r", IsActive: true,
		TriggerType: workflowModel.TriggerTaskCreated, MatchType: "any", ActionsJSON: `[{"type":"set_priority","priority":"high"}]`})
	if err != nil {
		t.Fatal(err)
	}
	r := func() *workflowModel.Run { return &workflowModel.Run{WorkflowID: id, Trigger: "task_created"} }
	var mu sync.Mutex
	won := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := workflowModel.ClaimRun(ctx, r(), "task_created:e1")
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("one event claimed %d runs", won)
	}

	turns := map[int64]bool{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := workflowModel.NextTurn(ctx, id)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			turns[n] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(turns) != 20 {
		t.Errorf("20 turns taken at once gave %d different ones", len(turns))
	}

	if n, _ := workflowModel.RunsSince(ctx, &id, time.Now().Add(-time.Minute)); n != 1 {
		t.Errorf("runs in the last minute: %d", n)
	}
	old := r()
	old.StartedAt = time.Now().Add(-100 * 24 * time.Hour)
	if _, err := workflowModel.ClaimRun(ctx, old, "task_created:old"); err != nil {
		t.Fatal(err)
	}
	if n, err := workflowModel.PurgeRuns(ctx, time.Now().Add(-workflowBusiness.RunsKept), 100); err != nil || n != 1 {
		t.Errorf("purged %d: %v", n, err)
	}
	if paused, _ := workflowModel.Pause(ctx, id, "too many"); !paused {
		t.Error("pausing an active rule")
	}
	if paused, _ := workflowModel.Pause(ctx, id, "too many"); paused {
		t.Error("a paused rule paused again, so its owner would be told twice")
	}
	if err := workflowModel.SetWorkflowActive(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	wf, _ := workflowModel.GetWorkflowByID(ctx, id)
	if !wf.IsActive || wf.PausedReason != nil {
		t.Errorf("turned back on: %+v", wf)
	}
	_ = postgresInit.DBConn
}
