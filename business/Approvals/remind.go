package business

// Due times and reminders (package 9 R2b, basecamp-25's H-5): a request with
// a due time reminds the approvers who haven't answered once, a day before
// it, and again when it passes, when the person who asked hears it too. A
// claimed pass: each stage is claimed in one statement (reminded_at), so a
// worker that loses the race sends nothing, and a request is never told
// twice.

import (
	"context"
	"fmt"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	"github.com/akashc777/OneCamp/models/taskread"
)

const (
	remindPass = "approvals.remind"
	// remindBatch bounds one pass's requests per stage.
	remindBatch = 200
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: remindPass, Every: 15 * time.Minute, ClaimsItsOwnWork: true, Run: func(ctx context.Context) error {
		return remind(ctx, now())
	}})
}

// Seams for tests: claiming what's due, and the tasks' names.
var (
	claimDue  = model.ClaimDue
	taskNames = taskDomain.TaskNames
)

func remind(ctx context.Context, at time.Time) error {
	soon, late, err := claimDue(ctx, at, remindBatch)
	if err != nil {
		return err
	}
	ids := []string{}
	for _, r := range append(append([]*model.DueRequest{}, soon...), late...) {
		if r.ResourceKind == model.KindTask {
			ids = append(ids, r.ResourceID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// Names read as nobody, as the time triggers read: a restricted task's
	// reminder says "A task", and its link opens it for whoever may see it.
	names, err := taskNames(taskread.AsNobody(ctx), ids)
	if err != nil {
		return err
	}
	// The live item names it by its key too, as the inbox will (taskKeys.NamedAll).
	keyed := taskKeys.NamedAll(ctx, names)
	named := func(r *model.DueRequest, from map[string]string) string {
		if name := from[r.ResourceID]; name != "" {
			return name
		}
		return "A task"
	}
	// Docs and files are named by their own readers (R2b-1b).
	for _, r := range soon {
		if r.ResourceKind != model.KindTask {
			continue
		}
		name := named(r, names)
		tellUnanswered(ctx, r, named(r, keyed), fmt.Sprintf("Reminder: %s waits for your approval, due within a day", name))
	}
	for _, r := range late {
		if r.ResourceKind != model.KindTask {
			continue
		}
		name, item := named(r, names), named(r, keyed)
		tellUnanswered(ctx, r, item, fmt.Sprintf("%s is past its due time and still waits for your approval", name))
		tellSeeing(ctx, r, dueNotice(r, item, "overdue", fmt.Sprintf("%s hasn't been approved by its due time", name)),
			[]notificationBusiness.NoticeRecipient{{UserUUID: r.RequestedBy.String(), Reason: "subject"}})
	}
	return nil
}

// tellUnanswered reminds a request's approvers who haven't answered.
func tellUnanswered(ctx context.Context, r *model.DueRequest, name, title string) {
	if len(r.Unanswered) == 0 {
		return
	}
	to := make([]notificationBusiness.NoticeRecipient, 0, len(r.Unanswered))
	for _, id := range r.Unanswered {
		to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "approver"})
	}
	tellSeeing(ctx, r, dueNotice(r, name, "reminder", title), to)
}

// tellSeeing tells those of to who may read the request's task now
// (seeingNow): a reminder comes long after the ask.
func tellSeeing(ctx context.Context, r *model.DueRequest, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) {
	if to = seeingNow(ctx, r.ResourceID, to); len(to) > 0 {
		tell(ctx, n, to)
	}
}

func dueNotice(r *model.DueRequest, name, stage, title string) notificationBusiness.Notice {
	return notificationBusiness.Notice{
		Kind: "work.approval.due", ResourceKind: workAccess.KindTask, ResourceUUID: r.ResourceID, ResourceTitle: name,
		Data:     map[string]string{"approval_id": r.ID.String(), "stage": stage},
		DedupKey: "approval-due|" + r.ID.String() + "|" + stage,
		Title:    title, Path: "/app/task/" + r.ResourceID,
	}
}
