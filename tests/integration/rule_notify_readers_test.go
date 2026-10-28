//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	"github.com/akashc777/OneCamp/helpers"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
)

// A rule's notice reaches only people who may open the task when it runs.
// Its notify step names people as the project stood when the rule was saved;
// ben has left it since, so he is told nothing of its tasks.
func TestARulesNoticeReachesNoOneWhoLeftTheProject(t *testing.T) {
	w := seedWorkWorld(t)
	rule := anasRule(t, w, workflowModel.TriggerTaskCreated, tellBenAndAna(w))
	benLeaves(t, w)

	webhookBusiness.DispatchEvent(context.Background(), "task.created", taskCreatedEvent(w))
	ruleRanOnce(t, w, rule)
	benToldNothingOf(t, w, w.task)
	if n := noticesTo(t, w, "ana", w.task); n != 1 {
		t.Errorf("ana, whose task it is, was told %d times, want once", n)
	}
}

// A rule that tells a task's creator tells no one who has left the project:
// ben made the task and has left since, so its move tells him nothing.
func TestARulesNoticeReachesNoCreatorWhoLeftTheProject(t *testing.T) {
	w := seedWorkWorld(t)
	rule := anasRule(t, w, workflowModel.TriggerTaskStatusChanged, workflowBusiness.WorkflowAction{
		Type: workflowBusiness.ActionNotify, Notify: workflowBusiness.NotifyCreator, Text: "{task} moved",
	})
	benLeaves(t, w)

	webhookBusiness.DispatchEvent(context.Background(), "task.status_changed", map[string]interface{}{
		"task_id": w.assigned, "project_id": w.project, "old_status": "todo", "new_status": "inProgress",
		"task_name": "Check the copy", "updated_by_uuid": w.people["ana"].String(),
	})
	ruleRanOnce(t, w, rule)
	benToldNothingOf(t, w, w.assigned)
}

// A task kept to some people after its event set the rule going. A batch's
// new tasks are marked open, so their events reach rules without a read of
// their restriction (business/Task CreateTasks), and a restriction can land
// before the run. On this line a rule's run reads its task with no reader,
// who sees no restricted task: it skips before any step, and no one is told.
func TestARulesNoticeReachesNoOneTheTaskKeepsOutWhenItRuns(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	rule := anasRule(t, w, workflowModel.TriggerTaskCreated, tellBenAndAna(w))
	ana := w.account("ana")
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	// ana keeps her task to the project's admins and herself: ben, a member,
	// is out.
	if err := workAccess.Restrict(ctx, task, ana.UserPostgresInfo.Id, nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}

	webhookBusiness.DispatchEvent(helpers.WithOpenTasks(ctx, uuid.MustParse(w.task)), "task.created", taskCreatedEvent(w))
	if run := runsOf(t, uuid.MustParse(w.project), rule, 1)[0]; run.Status != workflowModel.RunSkipped || len(run.Steps) != 0 {
		t.Fatalf("the run: %+v, want it skipped before any step", run)
	}
	benToldNothingOf(t, w, w.task)
	if n := noticesTo(t, w, "ana", w.task); n != 0 {
		t.Errorf("ana was told %d times of a task the run skipped", n)
	}
}

// anasRule is a rule ana makes in her project: on trigger, act.
func anasRule(t *testing.T, w *workWorld, trigger string, act workflowBusiness.WorkflowAction) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	workflowBusiness.Start(ctx)
	ana := w.account("ana")
	rule, err := workflowBusiness.CreateProjectRule(ctx, &ana, uuid.MustParse(w.project), workflowBusiness.WorkflowInput{
		Name: "Tell the team", IsActive: true, TriggerType: trigger, Actions: []workflowBusiness.WorkflowAction{act},
	})
	if err != nil {
		t.Fatalf("ana makes the rule: %v", err)
	}
	return rule.Id
}

// tellBenAndAna names ben and ana, both in the project when the rule is made.
func tellBenAndAna(w *workWorld) workflowBusiness.WorkflowAction {
	return workflowBusiness.WorkflowAction{
		Type: workflowBusiness.ActionNotify, Notify: workflowBusiness.NotifyPeople, Text: "{task} is new",
		UserIDs: []string{w.people["ben"].String(), w.people["ana"].String()},
	}
}

// benLeaves takes ben off the project, as its admin would.
func benLeaves(t *testing.T, w *workWorld) {
	t.Helper()
	var project struct {
		P []struct {
			UID string `json:"uid"`
		} `json:"p"`
	}
	if err := json.Unmarshal(w.dg.Query(t, `{ p(func: eq(project_uuid, "`+w.project+`")) { uid } }`), &project); err != nil || len(project.P) != 1 {
		t.Fatalf("the project's node: %v %+v", err, project)
	}
	ben := w.account("ben")
	if err := projectBusiness.RemoveMemberFromProject(context.Background(), ben.UserDgraphInfo.Uid, w.people["ben"].String(), project.P[0].UID, w.project); err != nil {
		t.Fatalf("ben leaves the project: %v", err)
	}
}

// taskCreatedEvent is ana's task being made, as the bus carries it.
func taskCreatedEvent(w *workWorld) map[string]interface{} {
	return map[string]interface{}{"task_id": w.task, "project_id": w.project, "name": "Write the launch post", "created_by": w.people["ana"].String()}
}

func ruleRanOnce(t *testing.T, w *workWorld, rule uuid.UUID) {
	t.Helper()
	if run := runsOf(t, uuid.MustParse(w.project), rule, 1)[0]; run.Status != workflowModel.RunOK {
		t.Fatalf("the run: %+v", run)
	}
}

// noticesTo is how many notices about the task who has.
func noticesTo(t *testing.T, w *workWorld, who, task string) int {
	t.Helper()
	var n int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid = $1 AND resource_uuid = $2`, w.people[who], task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// benToldNothingOf checks nothing of the task reached ben: no notice (so no
// live item, push or email) and nothing in his inbox.
func benToldNothingOf(t *testing.T, w *workWorld, task string) {
	t.Helper()
	if n := noticesTo(t, w, "ben", task); n != 0 {
		t.Errorf("ben, who may not open the task, was told about it %d times", n)
	}
	page := activityBusiness.WithNotices(context.Background(), dgraphModels.UnifiedActivityPagination{}, w.people["ben"].String(), time.Now().Add(time.Hour), 20)
	for _, a := range page.Activities {
		if a.Notice != nil && a.Notice.ResourceUUID == task {
			t.Errorf("ben's inbox has a notice about the task: %+v", a.Notice)
		}
	}
}
