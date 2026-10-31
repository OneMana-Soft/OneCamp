package business

// Answering a request: an approver approves, asks for changes or rejects,
// with a comment, which is also written on the task (so its history says who
// decided what). Enough approvals make the move, as the person who asked
// (who must still be able to make it: the rest of the rule still holds); one
// request for changes or one rejection closes the request. The person who
// asked, or a project admin, may withdraw it.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Refusals.
var (
	ErrNotApprover  = errors.New("you aren't one of this request's approvers")
	ErrNotOpen      = errors.New("that request is closed now")
	ErrNotYours     = errors.New("only the person who asked, or a project admin, can withdraw it")
	ErrBadVerdict   = errors.New("answer approve, changes or reject")
	ErrThroughToken = errors.New("approvals are answered in the app, not through a token")
	ErrNeedsComment = errors.New("say what needs to change")
)

// MaxComment bounds an answer's comment.
const MaxComment = 2000

// Seams for tests.
var (
	taskFor    = taskBusiness.TaskFor
	getAppr    = model.Get
	decide     = model.Decide
	closeAppr  = model.Close
	latestFor  = model.LatestFor
	pendingFor = model.PendingFor
	actorOf    = userDomain.ActorByUUID
	moveTask   = func(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, status string) error {
		_, _, err := taskWriter.Tasks.Apply(ctx, who, taskID, []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: quote(status)}}, 0)
		return err
	}
	comment = taskWriter.Comment
	tell    = notificationBusiness.Tell
	// tellRules tells the rules engine how a task's request ended, off the
	// answer's own path.
	tellRules = func(ctx context.Context, data map[string]interface{}) {
		told := context.WithoutCancel(ctx)
		helpers.GoSafeNamed("business/Approvals.tellRules", func() { webhookBusiness.DispatchEvent(told, "task.approval_decided", data) })
	}
)

func quote(s string) []byte {
	b := []byte(`"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`)
	return b
}

// View is a task's approval as the app shows it: the request, its answers,
// and what the reader may do with it.
type View struct {
	*model.Approval
	ToName      string `json:"to_name"`
	CanDecide   bool   `json:"can_decide"`
	CanWithdraw bool   `json:"can_withdraw"`
}

// ForTask is a task's newest approval request for who (nil when it has none).
func ForTask(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID) (*View, error) {
	task, s, err := taskFor(ctx, taskID, who, workAccess.NeedView)
	if err != nil {
		return nil, err
	}
	a, err := latestFor(ctx, taskID)
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return view(ctx, a, task, who, s), nil
}

func view(ctx context.Context, a *model.Approval, task *dgraphStruct.DgraphTask, who *userModels.UserInfo, s workAccess.Standing) *View {
	v := &View{Approval: a, ToName: a.ToStatus}
	projectID := ""
	if task != nil && task.Project != nil {
		projectID = task.Project.Uuid
	}
	if r, err := taskStatusBusiness.Resolve(ctx, projectID, a.ToStatus); err == nil {
		v.ToName = r.Display()
	}
	me := who.UserPostgresInfo.Id
	if a.State == model.Pending {
		v.CanDecide = slices.Contains(a.Approvers, me.String())
		v.CanWithdraw = a.RequestedBy == me || s.Can(workAccess.NeedManage)
	}
	return v
}

// Decide answers an open request as who.
func Decide(ctx context.Context, who *userModels.UserInfo, taskID, approvalID uuid.UUID, verdict, note string) (*View, error) {
	if helpers.ActsThroughToken(ctx) {
		return nil, ErrThroughToken
	}
	note = strings.TrimSpace(note)
	switch verdict {
	case model.VerdictApprove, model.VerdictReject:
	case model.VerdictChanges:
		if note == "" {
			return nil, ErrNeedsComment
		}
	default:
		return nil, ErrBadVerdict
	}
	if len(note) > MaxComment {
		return nil, fmt.Errorf("a comment is at most %d characters", MaxComment)
	}
	task, s, err := taskFor(ctx, taskID, who, workAccess.NeedView)
	if err != nil {
		return nil, err
	}
	a, err := getAppr(ctx, approvalID)
	if err != nil || a.TaskID != taskID {
		return nil, ErrNotOpen
	}
	if a.State != model.Pending {
		return nil, ErrNotOpen
	}
	me := who.UserPostgresInfo.Id
	if !slices.Contains(a.Approvers, me.String()) {
		return nil, ErrNotApprover
	}
	a, err = decide(ctx, approvalID, me, verdict, note)
	if errors.Is(err, model.ErrNotFound) {
		return nil, ErrNotOpen
	}
	if err != nil {
		return nil, err
	}
	toName := view(ctx, a, task, who, s).ToName
	// The answer, on the task: who decided what, in its history.
	said := map[string]string{model.VerdictApprove: "Approved", model.VerdictChanges: "Asked for changes", model.VerdictReject: "Rejected"}[verdict]
	line := fmt.Sprintf("%s moving it to %s.", said, toName)
	if note != "" {
		line += " " + note
	}
	if _, err := comment(helpers.WithWorkflowGenerated(ctx), who, taskID, line); err != nil {
		helpers.LogErrorWithContext(ctx, "Approvals: comment on %s: %v", taskID, err)
	}

	actor := who.UserDgraphInfo
	settle(ctx, a, task, &actor, who.UserDgraphInfo.DisplayName(), verdict, toName)
	return view(ctx, a, task, who, s), nil
}

// complete is whether a request has all it waits for: enough of its approvers,
// and, when it names client links, a client's approval too.
func complete(a *model.Approval) bool {
	members, client := 0, false
	for _, d := range a.Decisions {
		if d.Verdict != model.VerdictApprove {
			continue
		}
		if d.Client {
			client = true
		} else {
			members++
		}
	}
	return members >= a.Needed && (!a.Client || client)
}

// settle closes a request after an answer, when the answer closes it: one
// request for changes or one rejection ends it, and the last approval it
// waits for approves it, making a held move as the person who asked.
// Whoever answered (a member, or a client by the name they gave) is named
// to the person who asked, and rules hear how it ended.
func settle(ctx context.Context, a *model.Approval, task *dgraphStruct.DgraphTask, actor *dgraphStruct.DgraphUser, byName, verdict, toName string) {
	switch {
	case verdict == model.VerdictReject || verdict == model.VerdictChanges:
		state, said := model.Rejected, "rejected"
		if verdict == model.VerdictChanges {
			state, said = model.Changes, "asked for changes to"
		}
		if ok, err := closeAppr(ctx, a.ID, state); err == nil && ok {
			a.State = state
			what := task.Name
			if a.ToStatus != "" {
				what = fmt.Sprintf("moving %s to %s", task.Name, toName)
			}
			tellRequester(ctx, a, task, actor, fmt.Sprintf("%s %s %s", byName, said, what))
			answered(ctx, a, task, actor, byName, verdict)
		}
	case complete(a):
		if ok, err := closeAppr(ctx, a.ID, model.Approved); err == nil && ok {
			a.State = model.Approved
			finish(ctx, a, task, actor, toName)
			answered(ctx, a, task, actor, byName, verdict)
		}
	}
}

// finish tells the person who asked that a request is approved, and makes a
// held move first, as them.
func finish(ctx context.Context, a *model.Approval, task *dgraphStruct.DgraphTask, actor *dgraphStruct.DgraphUser, toName string) {
	if a.ToStatus == "" {
		tellRequester(ctx, a, task, actor, fmt.Sprintf("%s is approved", task.Name))
		return
	}
	requester, err := actorOf(ctx, a.RequestedBy)
	msg := fmt.Sprintf("%s is approved and moved to %s", task.Name, toName)
	if err == nil {
		err = moveTask(withApproved(ctx, a.ID), requester, a.TaskID, a.ToStatus)
	}
	if err != nil {
		why := err.Error()
		if r, ok := taskBusiness.RefusedStatus(err); ok {
			why = r.Why
		}
		helpers.LogErrorWithContext(ctx, "Approvals: approved move of %s: %v", a.TaskID, err)
		msg = fmt.Sprintf("%s is approved, but it couldn't move to %s: %s", task.Name, toName, why)
	}
	tellRequester(ctx, a, task, actor, msg)
}

// answered tells rules how a task's request ended (approval_decided): the
// task, its project, the outcome, the move it held and the last answer, by
// the keys every task event uses (task_id, project_id, updated_by_uuid: none
// for a client).
func answered(ctx context.Context, a *model.Approval, task *dgraphStruct.DgraphTask, actor *dgraphStruct.DgraphUser, byName, verdict string) {
	if a.ResourceKind != model.KindTask {
		return
	}
	comment := ""
	for _, d := range a.Decisions {
		if d.Verdict == verdict {
			comment = d.Comment
		}
	}
	by := ""
	if actor != nil {
		by = actor.Uuid
	}
	data := map[string]interface{}{
		"approval_id": a.ID.String(), "task_id": a.TaskID.String(), "task_name": task.Name, "project_id": a.ProjectID.String(),
		"outcome": a.State, "to_status": a.ToStatus, "approver": byName, "comment": comment, "requested_by": a.RequestedBy.String(),
		"updated_by_uuid": by,
	}
	tellRules(ctx, data)
}

// Withdraw takes back an open request.
func Withdraw(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID) error {
	if helpers.ActsThroughToken(ctx) {
		return ErrThroughToken
	}
	_, s, err := taskFor(ctx, taskID, who, workAccess.NeedView)
	if err != nil {
		return err
	}
	a, err := pendingFor(ctx, taskID)
	if errors.Is(err, model.ErrNotFound) {
		return ErrNotOpen
	}
	if err != nil {
		return err
	}
	if a.RequestedBy != who.UserPostgresInfo.Id && !s.Can(workAccess.NeedManage) {
		return ErrNotYours
	}
	ok, err := closeAppr(ctx, a.ID, model.Withdrawn)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotOpen
	}
	return nil
}

// tellApprovers asks a request's approvers for their answer.
func tellApprovers(ctx context.Context, a *model.Approval, task *dgraphStruct.DgraphTask, by *dgraphStruct.DgraphUser, toName string) {
	recipients := make([]notificationBusiness.NoticeRecipient, 0, len(a.Approvers))
	for _, id := range a.Approvers {
		recipients = append(recipients, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "approver"})
	}
	tell(ctx, notificationBusiness.Notice{
		Kind: "work.approval.requested", ResourceKind: workAccess.KindTask, ResourceUUID: task.Uuid, ResourceTitle: taskKeys.NamedTask(ctx, task.Uuid, task.Name),
		Actor: by, Data: map[string]string{"approval_id": a.ID.String(), "to_status": a.ToStatus},
		DedupKey: "approval|" + a.ID.String(),
		Title:    askTitle(by.DisplayName(), a, task.Name, toName),
		Path:     "/app/task/" + task.Uuid,
		Push:     &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_APPROVAL, ID: task.Uuid},
	}, recipients)
}

// askTitle is what an approver is asked: a held move, or the item itself.
func askTitle(by string, a *model.Approval, name, toName string) string {
	if a.ToStatus != "" {
		return fmt.Sprintf("%s asks you to approve moving %s to %s", by, name, toName)
	}
	return fmt.Sprintf("%s asks you to approve %s", by, name)
}

// tellRequester tells the person who asked how it ended, while they may read
// the task (seeingNow). actor is nil for a client, who is no member.
func tellRequester(ctx context.Context, a *model.Approval, task *dgraphStruct.DgraphTask, actor *dgraphStruct.DgraphUser, title string) {
	to := seeingNow(ctx, task.Uuid, []notificationBusiness.NoticeRecipient{{UserUUID: a.RequestedBy.String(), Reason: "subject"}})
	if len(to) == 0 {
		return
	}
	tell(ctx, notificationBusiness.Notice{
		Kind: "work.approval.decided", ResourceKind: workAccess.KindTask, ResourceUUID: task.Uuid, ResourceTitle: taskKeys.NamedTask(ctx, task.Uuid, task.Name),
		Actor: actor, Data: map[string]string{"approval_id": a.ID.String(), "state": a.State},
		DedupKey: "approval-decided|" + a.ID.String() + "|" + a.State,
		Title:    title, Path: "/app/task/" + task.Uuid,
		Push: &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_APPROVAL, ID: task.Uuid},
	}, to)
}
