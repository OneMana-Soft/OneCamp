package business

// Runs: every time a rule acts is a row (workflow_runs), claimed by the
// event's key before anything happens, so an event the hand-off delivers
// twice runs once; written with each step, what it changed and what it
// replaced, so the history says what happened and a run can be undone.
//
// Runaway guards (PROGRAM §16 AU5): a rule's writes never trigger rules
// (helpers.WithWorkflowGenerated); a rule that runs more than perMinute
// times a minute or perHour an hour stops itself and its owner is told; the
// workspace runs at most workspacePerHour an hour, after which runs are
// recorded as skipped until the hour turns.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// The limits; variables so tests can lower them.
var (
	perMinute        = 60
	perHour          = 1000
	workspacePerHour = 10000
)

// Seams for tests: the store and the reads a run makes.
var (
	claimRun  = workflowModel.ClaimRun
	finishRun = workflowModel.FinishRun
	runsSince = workflowModel.RunsSince
	pauseRule = workflowModel.Pause
	recordRun = workflowModel.RecordRun
	loadActor = func(ctx context.Context, id string) (*userModels.UserInfo, error) {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, err
		}
		return userDomain.ActorByUUID(ctx, u)
	}
	readTask = func(ctx context.Context, taskID string, who *userModels.UserInfo) (*dgraphStruct.DgraphTask, error) {
		id, err := uuid.Parse(taskID)
		if err != nil {
			return nil, err
		}
		t, _, err := taskBusiness.TaskFor(ctx, id, who, workAccess.NeedView)
		return t, err
	}
)

// eventKey is what makes one event's run one run: the event's own id (set
// when it was dispatched), else its contents.
func eventKey(trigger string, data map[string]interface{}) string {
	if id, _ := data["event_id"].(string); id != "" {
		return trigger + ":" + id
	}
	b, _ := json.Marshal(data) // keys sorted, so the same event is the same text
	sum := sha256.Sum256(b)
	return trigger + ":" + hex.EncodeToString(sum[:16])
}

// limited says whether a rule may run now: "" if it may, else why not, and
// whether the rule stops itself.
func limited(ctx context.Context, cw *compiledWorkflow, now time.Time) (string, bool) {
	id := cw.id
	if n, err := runsSince(ctx, &id, now.Add(-time.Minute)); err == nil && n >= perMinute {
		return fmt.Sprintf("Paused: it ran %d times in a minute", n), true
	}
	if n, err := runsSince(ctx, &id, now.Add(-time.Hour)); err == nil && n >= perHour {
		return fmt.Sprintf("Paused: it ran %d times in an hour", n), true
	}
	if n, err := runsSince(ctx, nil, now.Add(-time.Hour)); err == nil && n >= workspacePerHour {
		return "Skipped: the workspace's rules ran their most this hour", false
	}
	return "", false
}

// runWorkflow runs a matched rule for one event: as its owner, once per
// event, on the branch its conditions choose, and records the run.
func runWorkflow(ctx context.Context, cw *compiledWorkflow, ev triggerEvent) {
	allowed := access.allows(ctx, cw.createdBy, ev)
	if !allowed && ev.taskID == "" {
		return // a channel its owner can't read: not theirs to hear
	}
	now := clock()
	run := &workflowModel.Run{WorkflowID: cw.id, Trigger: cw.triggerType, SubjectID: ev.subjectID(), Subject: ev.text, StartedAt: now}
	if pid, err := uuid.Parse(ev.projectID); err == nil {
		run.ProjectID = &pid
	}
	if ev.eventKey == "" {
		ev.eventKey = eventKey(cw.triggerType, map[string]interface{}{"subject": ev.subjectID(), "at": now.UnixNano()})
	}
	skip := func(reason string) {
		run.Status, run.Error = workflowModel.RunSkipped, reason
		run.FinishedAt = &now
		if _, err := claimRun(ctx, run, ev.eventKey); err != nil {
			helpers.LogErrorWithContext(ctx, "Workflow %s skip record: %v", cw.id, err)
		}
	}

	actions := cw.actions
	var rc *runCtx
	if !allowed {
		// A project rule whose owner left the project: said in its history,
		// so its admins see why it stopped acting.
		skip("Skipped: the rule's owner isn't in the project any more")
		return
	}
	if ev.taskID != "" {
		owner, err := loadActor(ctx, cw.createdBy)
		if err != nil {
			skip("Skipped: the rule's owner can't act any more")
			return
		}
		task, err := readTask(ctx, ev.taskID, owner)
		if err != nil || task == nil {
			skip("Skipped: the rule's owner can't see this task")
			return
		}
		run.Subject = task.Name
		rc = &runCtx{owner: owner, task: task}
		if cw.conditions.hold(factsOf(task), now, cw.loc) {
			run.Branch = "then"
		} else {
			run.Branch, actions = "else", cw.elseActions
		}
		if len(actions) == 0 {
			return // the conditions chose no actions: nothing happened
		}
	}

	if reason, stop := limited(ctx, cw, now); reason != "" {
		if stop {
			cw.stop(ctx, reason)
		}
		skip(reason)
		return
	}
	run.Status = workflowModel.RunRunning
	claimed, err := claimRun(ctx, run, ev.eventKey)
	if err != nil || !claimed {
		return // stored nothing, or this event already has its run
	}
	if rc != nil {
		rc.runID = run.ID
	}

	actionCtx := helpers.WithWorkflowGenerated(ctx)
	limit := MaxActionsPerWorkflow
	if rc != nil {
		limit = MaxActionsPerBranch
	}
	var firstErr string
	for i, act := range actions {
		if i >= limit {
			break
		}
		var step workflowModel.Step
		if rc != nil && isTaskAction(act.Type) {
			step = cw.runTaskAction(actionCtx, act, ev, rc, i)
		} else {
			step = workflowModel.Step{Type: act.Type, Status: "ok"}
			if made, err := cw.runAction(actionCtx, act, ev, rc); err != nil {
				helpers.LogErrorWithContext(ctx, "Workflow %s action %q failed: %v", cw.id, act.Type, err)
				step.Status, step.Detail = "failed", plainError(err)
			} else {
				step.Made = made
			}
		}
		if step.Status == "failed" && firstErr == "" {
			firstErr = step.Detail
		}
		run.Steps = append(run.Steps, step)
	}
	run.Status, run.Error = workflowModel.RunOK, firstErr
	if firstErr != "" {
		run.Status = workflowModel.RunFailed
	}
	if err := finishRun(ctx, run); err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow %s finish run: %v", cw.id, err)
	}
	if err := recordRun(ctx, cw.id, now, firstErr); err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow %s RecordRun err: %v", cw.id, err)
	}
}

// stop pauses a rule that ran away and tells its owner, once.
func (cw *compiledWorkflow) stop(ctx context.Context, reason string) {
	paused, err := pauseRule(ctx, cw.id, reason)
	if err != nil || !paused {
		return
	}
	_ = reloadRules(ctx)
	path := "/app/settings/workflows"
	if cw.projectID != "" {
		path = "/app/project/" + cw.projectID + "?rules=1"
	}
	tell(ctx, notificationBusiness.Notice{
		Kind: "work.rule.paused", ResourceKind: "rule", ResourceUUID: cw.id.String(), ResourceTitle: cw.name,
		Data:     map[string]string{"project_id": cw.projectID},
		DedupKey: "rule-paused|" + cw.id.String() + "|" + clock().UTC().Format(time.RFC3339),
		Title:    "Your rule “" + cw.name + "” stopped itself",
		Body:     reason + ". Turn it back on when it's fixed.",
		Path:     path,
	}, []notificationBusiness.NoticeRecipient{{UserUUID: cw.createdBy, Reason: "owner"}})
}
