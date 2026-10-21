package business

// The actions a rule takes on the task it is about. Every change goes
// through the Task Writer as the rule's owner (business/TaskWriter): the same
// check, the same history line, the same notices as the change made by hand.
// Each step keeps what it replaced, so a run can be undone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// The task actions.
const (
	ActionSetStatus   = "set_status"
	ActionSetPriority = "set_priority"
	ActionAssign      = "assign"
	ActionSetDue      = "set_due"
	ActionComment     = "add_comment"
	ActionAddSubtask  = "add_subtask"
	ActionNotify      = "notify"
)

// MaxActionsPerBranch bounds a task rule's actions in each branch.
const MaxActionsPerBranch = 10

// How assign picks someone.
const (
	AssignPerson      = "person"
	AssignRoundRobin  = "round_robin"
	AssignLeastLoaded = "least_loaded"
	AssignCreator     = "creator"
	AssignActor       = "actor" // whoever made the change the rule heard
	AssignNobody      = "nobody"
)

// Whom notify tells.
const (
	NotifyAssignee = "assignee"
	NotifyCreator  = "creator"
	NotifyActor    = "actor"
	NotifyPeople   = "people"
)

// maxPool bounds the people an assign or notify names.
const maxPool = 50

var taskActions = []string{ActionSetStatus, ActionSetPriority, ActionAssign, ActionSetDue, ActionComment, ActionAddSubtask, ActionNotify}

func isTaskAction(t string) bool { return slices.Contains(taskActions, t) }

// taskTriggers are the triggers about one task.
var taskTriggers = []string{workflowModel.TriggerTaskCreated, workflowModel.TriggerTaskStatusChanged,
	workflowModel.TriggerTaskAssigned, workflowModel.TriggerTaskFieldChanged}

func isTaskTrigger(t string) bool { return slices.Contains(taskTriggers, t) }

// validateTaskAction checks a task action as it is saved. A status is
// resolved against the rule's project later (resolveStatuses), where the
// project is known.
func validateTaskAction(act *WorkflowAction) error {
	act.Text = strings.TrimSpace(act.Text)
	act.TaskName = strings.TrimSpace(act.TaskName)
	switch act.Type {
	case ActionSetStatus:
		act.Status = strings.TrimSpace(act.Status)
		if act.Status == "" {
			return fmt.Errorf("pick the status to move the task to")
		}
	case ActionSetPriority:
		act.Priority = strings.ToLower(strings.TrimSpace(act.Priority))
		if !taskPriority.Is(act.Priority) {
			return fmt.Errorf("a priority is urgent, high, medium or low")
		}
	case ActionAssign:
		switch act.Assign {
		case AssignPerson:
			if len(act.UserIDs) != 1 {
				return fmt.Errorf("pick the person to assign")
			}
		case AssignRoundRobin, AssignLeastLoaded:
		case AssignCreator, AssignActor, AssignNobody:
			act.UserIDs = nil
		default:
			return fmt.Errorf("assign to a person, by turns, to whoever has least, to its creator, or to nobody")
		}
	case ActionSetDue:
		if act.DueInDays != nil && (*act.DueInDays < 0 || *act.DueInDays > 365) {
			return fmt.Errorf("a due date is set 0 to 365 days ahead")
		}
	case ActionComment:
		if act.Text == "" {
			return fmt.Errorf("write the comment")
		}
		if len(act.Text) > taskWriter.MaxCommentLength {
			return fmt.Errorf("a comment is at most %d characters", taskWriter.MaxCommentLength)
		}
	case ActionAddSubtask:
		if act.TaskName == "" {
			return fmt.Errorf("name the sub-task")
		}
		if len(act.TaskName) > 200 {
			return fmt.Errorf("a sub-task's name is at most 200 characters")
		}
	case ActionNotify:
		if act.Text == "" {
			return fmt.Errorf("write what the notice says")
		}
		if len(act.Text) > 300 {
			return fmt.Errorf("a notice is at most 300 characters")
		}
		switch act.Notify {
		case NotifyAssignee, NotifyCreator, NotifyActor:
			act.UserIDs = nil
		case NotifyPeople:
			if len(act.UserIDs) == 0 {
				return fmt.Errorf("pick who to tell")
			}
		default:
			return fmt.Errorf("tell the assignee, the creator, whoever made the change, or people you pick")
		}
	}
	if len(act.UserIDs) > maxPool {
		return fmt.Errorf("at most %d people", maxPool)
	}
	for i, id := range act.UserIDs {
		u, err := uuid.Parse(strings.TrimSpace(id))
		if err != nil {
			return fmt.Errorf("pick people from the project")
		}
		act.UserIDs[i] = u.String()
	}
	return nil
}

// checkTaskActionsIn checks a project rule's actions against its project:
// a status it has, people who are in it. Statuses are stored as their keys
// or ids, so a rename never breaks a rule.
func checkTaskActionsIn(ctx context.Context, projectID string, branches ...[]WorkflowAction) error {
	for _, actions := range branches {
		for i := range actions {
			act := &actions[i]
			if act.Type == ActionSetStatus {
				r, err := taskStatusBusiness.Resolve(ctx, projectID, act.Status)
				if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
					return fmt.Errorf("%q is not a status of this project. %s", act.Status, taskStatusBusiness.Describe(ctx, projectID))
				}
				if err != nil {
					return err
				}
				act.Status = r.Value()
			}
			for _, id := range act.UserIDs {
				ok, err := memberOf(ctx, id, projectID)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("everyone a rule names must be in the project")
				}
			}
		}
	}
	return nil
}

// Seams for tests: the writer, the reads and the people.
var (
	applyOps = func(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, ops []taskWriter.Op) error {
		_, _, err := taskWriter.Tasks.Apply(ctx, who, taskID, ops, 0)
		return err
	}
	makeComment = taskWriter.Comment
	makeSubTask = taskWriter.MakeSubTask
	makeTask    = taskWriter.MakeTask
	memberOf    = projectBusiness.HasMember
	// membersOf is a project's members, each saying whether they are a bot
	// (an AI teammate): "everyone" never includes one, naming one does.
	membersOf = func(ctx context.Context, projectID string, who *userModels.UserInfo) (map[string]bool, error) {
		p, err := projectBusiness.GetDgraphProjectMemberInfo(ctx, projectID, who.UserDgraphInfo.Uid)
		if err != nil || p == nil {
			return nil, err
		}
		out := make(map[string]bool, len(p.Members))
		for _, m := range p.Members {
			if m != nil && m.Uuid != "" {
				out[m.Uuid] = m.IsBot
			}
		}
		return out, nil
	}
	openCounts = taskDomain.OpenTaskCountsByAssignee
	nextTurn   = workflowModel.NextTurn
	tell       = notificationBusiness.Tell
	clock      = time.Now
)

// runCtx is what a task rule's actions share: who acts, and the task as the
// rule saw it, kept up to date as each step changes it.
type runCtx struct {
	owner *userModels.UserInfo
	task  *dgraphStruct.DgraphTask
	runID uuid.UUID
}

func (rc *runCtx) taskID() uuid.UUID {
	id, _ := uuid.Parse(rc.task.Uuid)
	return id
}

func (rc *runCtx) projectID() string {
	if rc.task.Project != nil {
		return rc.task.Project.Uuid
	}
	return ""
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// runTaskAction takes one task action and answers its step.
func (cw *compiledWorkflow) runTaskAction(ctx context.Context, act WorkflowAction, ev triggerEvent, rc *runCtx, idx int) workflowModel.Step {
	step := workflowModel.Step{Type: act.Type, Status: "ok", TaskID: rc.task.Uuid}
	fail := func(err error) workflowModel.Step {
		step.Status = "failed"
		step.Detail = plainError(err)
		return step
	}
	switch act.Type {
	case ActionSetStatus:
		before := taskStatusBusiness.Of(rc.task)
		if before.Value() == act.Status {
			step.Status, step.Detail = "skipped", "already there"
			return step
		}
		if err := applyOps(ctx, rc.owner, rc.taskID(), []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: raw(act.Status)}}); err != nil {
			return fail(err)
		}
		step.Field, step.Before, step.After = "status", raw(before.Value()), raw(act.Status)
		if r, err := taskStatusBusiness.Resolve(ctx, rc.projectID(), act.Status); err == nil {
			rc.task.Status = r.Category
			cid, cname := r.CustomID, r.CustomName
			rc.task.CustomStatus, rc.task.CustomStatusName = &cid, &cname
			step.Detail = "moved to " + r.Display()
		}
	case ActionSetPriority:
		if rc.task.Priority == act.Priority {
			step.Status, step.Detail = "skipped", "already "+act.Priority
			return step
		}
		if err := applyOps(ctx, rc.owner, rc.taskID(), []taskWriter.Op{{Kind: taskWriter.OpPriority, Value: raw(act.Priority)}}); err != nil {
			return fail(err)
		}
		step.Field, step.Before, step.After = "priority", raw(rc.task.Priority), raw(act.Priority)
		step.Detail = "priority " + act.Priority
		rc.task.Priority = act.Priority
	case ActionAssign:
		to, err := cw.pickAssignee(ctx, act, ev, rc)
		if err != nil {
			return fail(err)
		}
		before := ""
		if rc.task.Assignee != nil {
			before = rc.task.Assignee.Uuid
		}
		if before == to {
			step.Status, step.Detail = "skipped", "already theirs"
			return step
		}
		if err := applyOps(ctx, rc.owner, rc.taskID(), []taskWriter.Op{{Kind: taskWriter.OpAssignee, Value: raw(to)}}); err != nil {
			return fail(err)
		}
		step.Field, step.Before, step.After = "assignee", raw(before), raw(to)
		if to == "" {
			rc.task.Assignee = nil
			step.Detail = "unassigned"
		} else {
			rc.task.Assignee = &dgraphStruct.DgraphUser{Uuid: to}
			step.Detail = "assigned"
		}
	case ActionSetDue:
		var due *time.Time
		if act.DueInDays != nil {
			d := dayStart(clock(), cw.loc).AddDate(0, 0, *act.DueInDays)
			due = &d
		}
		before := wireTime(rc.task.DueDate)
		after := wireTime(due)
		if before == after {
			step.Status, step.Detail = "skipped", "already due then"
			return step
		}
		var value any
		if due != nil {
			value = after
		}
		if err := applyOps(ctx, rc.owner, rc.taskID(), []taskWriter.Op{{Kind: taskWriter.OpDue, Value: raw(value)}}); err != nil {
			return fail(err)
		}
		step.Field, step.Before, step.After = "due", raw(before), raw(after)
		rc.task.DueDate = due
		if due == nil {
			step.Detail = "due date cleared"
		} else {
			step.Detail = "due " + due.Format("2 Jan")
		}
	case ActionComment:
		id, err := makeComment(ctx, rc.owner, rc.taskID(), applyVars(act.Text, ev.vars))
		if err != nil {
			return fail(err)
		}
		step.Made, step.Detail = id, "commented"
	case ActionAddSubtask:
		sub, err := makeSubTask(ctx, rc.owner, rc.task, applyVars(act.TaskName, ev.vars))
		if err != nil {
			return fail(err)
		}
		if sub != nil {
			step.Made = sub.Uuid
		}
		step.Detail = "added a sub-task"
	case ActionNotify:
		people := cw.whomToTell(act, ev, rc)
		if len(people) == 0 {
			step.Status, step.Detail = "skipped", "nobody to tell"
			return step
		}
		recipients := make([]notificationBusiness.NoticeRecipient, 0, len(people))
		for _, p := range people {
			recipients = append(recipients, notificationBusiness.NoticeRecipient{UserUUID: p, Reason: "rule"})
		}
		told := tell(ctx, notificationBusiness.Notice{
			Kind: "work.rule", ResourceKind: "task", ResourceUUID: rc.task.Uuid, ResourceTitle: rc.task.Name,
			Data:     map[string]string{"rule_id": cw.id.String(), "run_id": rc.runID.String()},
			DedupKey: fmt.Sprintf("rule|%s|%d", rc.runID, idx),
			Title:    applyVars(act.Text, ev.vars), Body: cw.name,
			Path: "/app/task/" + rc.task.Uuid,
			Push: &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK, ID: rc.task.Uuid},
		}, recipients)
		step.Detail = fmt.Sprintf("told %d", len(told))
	}
	return step
}

// pickAssignee is who an assign action gives the task to: "" for nobody.
func (cw *compiledWorkflow) pickAssignee(ctx context.Context, act WorkflowAction, ev triggerEvent, rc *runCtx) (string, error) {
	switch act.Assign {
	case AssignNobody:
		return "", nil
	case AssignPerson:
		return act.UserIDs[0], nil
	case AssignCreator:
		if rc.task.CreatedBy == nil || rc.task.CreatedBy.Uuid == "" {
			return "", fmt.Errorf("the task has no creator to give it to")
		}
		return rc.task.CreatedBy.Uuid, nil
	case AssignActor:
		if ev.targetUserUUID == "" {
			return "", fmt.Errorf("nobody made the change")
		}
		return ev.targetUserUUID, nil
	}
	pool, err := cw.pool(ctx, act, rc)
	if err != nil {
		return "", err
	}
	if len(pool) == 0 {
		return "", fmt.Errorf("nobody in the project to give it to")
	}
	if act.Assign == AssignLeastLoaded {
		// Counted as the rule's owner sees the project: a restricted task
		// they may not see isn't counted (models/taskread).
		readCtx := taskread.AsNobody(ctx)
		if rc.owner != nil {
			readCtx = taskread.ActingFor(ctx, rc.owner.UserPostgresInfo.Id.String())
		}
		counts, err := openCounts(readCtx, rc.projectID())
		if err != nil {
			return "", err
		}
		best := pool[0]
		for _, p := range pool[1:] {
			if counts[p] < counts[best] {
				best = p
			}
		}
		return best, nil
	}
	n, err := nextTurn(ctx, cw.id)
	if err != nil {
		return "", err
	}
	return pool[int((n-1)%int64(len(pool)))], nil
}

// pool is the people an assign chooses among: those it names who are still
// in the project, or everyone in it; in a fixed order, so turns go round.
func (cw *compiledWorkflow) pool(ctx context.Context, act WorkflowAction, rc *runCtx) ([]string, error) {
	members, err := membersOf(ctx, rc.projectID(), rc.owner)
	if err != nil {
		return nil, err
	}
	var out []string
	if len(act.UserIDs) > 0 {
		for _, id := range act.UserIDs {
			if _, in := members[id]; in {
				out = append(out, id)
			}
		}
	} else {
		for id, bot := range members {
			if !bot {
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

// whomToTell is who a notify action tells.
func (cw *compiledWorkflow) whomToTell(act WorkflowAction, ev triggerEvent, rc *runCtx) []string {
	switch act.Notify {
	case NotifyAssignee:
		if rc.task.Assignee != nil && rc.task.Assignee.Uuid != "" {
			return []string{rc.task.Assignee.Uuid}
		}
	case NotifyCreator:
		if rc.task.CreatedBy != nil && rc.task.CreatedBy.Uuid != "" {
			return []string{rc.task.CreatedBy.Uuid}
		}
	case NotifyActor:
		if ev.targetUserUUID != "" {
			return []string{ev.targetUserUUID}
		}
	case NotifyPeople:
		return act.UserIDs
	}
	return nil
}

// wireTime is a time as a step keeps it, "" for none.
func wireTime(t *time.Time) string {
	if t == nil || t.Year() <= 1970 {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// plainError is an error as a run's history shows it.
func plainError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "tasks: ")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
