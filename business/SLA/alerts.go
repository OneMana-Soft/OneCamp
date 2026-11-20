package business

// Alerts (linear-24 H2): the projects.sla sweep (business/WorkSweep, every
// minute, one instance a pass) tells a task's assignee once when its timer goes
// at risk (a business day before its due on the project's clock, or the last
// quarter of a shorter target: DueOf) and once when it breaches (past due while
// running); a timer found past due is told as breached alone.
//
// A pass reads a page of timers whose alert is due (one read), their tasks
// (one read) and their projects' switches (one read), then claims the page
// (one conditional UPDATE, as the check-ins' Claim) and tells only what it
// claimed: two sweeps, or two servers, never tell one twice. A crash between
// the claim and the telling loses that alert; it is never told twice.
//
// Who is told is the one hearer check urgent.go's notices take (business/Task
// AssigneeHears): the assignee alone, while their setting for the project
// hears and they may read the task as one of the project's people, and never a
// guest (guests read no SLA state, lay.go). They are told as every notice tells
// (business/Notification Tell): in their Inbox, by push through their holds,
// and by email by their "tasks assigned to you" switch. The notice's key names
// the due, so a moved due (PutTimer clears what was told) tells again.
//
// A task's alert told or not, while its project's SLAs are on and the task is
// live and open, goes out as its Resource Event (sla.at_risk, sla.breached).
// An alert due while the project's SLAs are off is claimed and told nobody:
// switching them back on replays nothing. So is one of a task deleted, done or
// cancelled whose timer the sink hasn't settled yet.

import (
	"context"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workSweep "github.com/akashc777/OneCamp/business/WorkSweep"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// The Inbox kinds of an SLA's alerts.
const (
	NoticeAtRisk   = "task.sla.at_risk"
	NoticeBreached = "task.sla.breached"
)

// alertPage is the most alerts a pass tells: the rest wait for the next.
const alertPage = 200

// Seams for tests: the due alerts, their claim, their tasks, who hears, how
// they are told and how the events go out.
var (
	dueAlerts   = model.Due
	claimAlerts = model.Claim
	alertTasks  = taskDomain.GetDgraphTasksForAlerts
	hears       = taskBusiness.AssigneeHears
	tellAlert   = notificationBusiness.Tell
	emitAlert   = func(ctx context.Context, e resourceEventBusiness.Event) { resourceEventBusiness.Bus.Emit(ctx, e) }
)

func init() { workSweep.SLA = sweepAlerts }

// sweepAlerts is one pass of the projects.sla sweep (the file's comment).
func sweepAlerts(ctx context.Context) error {
	now := clock()
	due, err := dueAlerts(ctx, now, alertPage)
	if err != nil || len(due) == 0 {
		return err
	}
	ids := make([]uuid.UUID, 0, len(due))
	names := make([]string, 0, len(due))
	projects := make([]string, 0, len(due))
	seen := map[string]bool{}
	for _, a := range due {
		ids, names = append(ids, a.TaskID), append(names, a.TaskID.String())
		if p := a.ProjectID.String(); !seen[p] {
			seen[p], projects = true, append(projects, p)
		}
	}
	read := helpers.WithSystemRead(ctx)
	// The tasks read whole, restricted ones too (no reader filters a sweep);
	// each alert is told only to an assignee who may read its task (hears).
	tasks, err := alertTasks(taskread.WithSystem(read), names)
	if err != nil {
		return err
	}
	// A failed read of the switches claims nothing: the next sweep tries again.
	on, err := onIn(ctx, projects)
	if err != nil {
		return err
	}
	claimed, err := claimAlerts(ctx, ids, now)
	if err != nil {
		return err
	}
	for _, a := range claimed {
		alert(read, a, tasks[a.TaskID.String()], on[a.ProjectID.String()], now)
	}
	return nil
}

// alert tells of one claimed alert, while its project's SLAs are on and its
// task is live and open: its event, and its assignee when they hear.
func alert(ctx context.Context, a model.Alert, t *dgraphStruct.DgraphTask, on bool, now time.Time) {
	if !on || !open(t) {
		return
	}
	kind := resourceEventBusiness.SLAAtRisk
	if a.Breach {
		kind = resourceEventBusiness.SLABreached
	}
	emitAlert(ctx, resourceEventBusiness.Event{Kind: kind, ResourceKind: "task", ResourceUUID: a.TaskID.String(),
		Actor: resourceEventBusiness.Actor{System: true}, At: now,
		Data: map[string]string{"project_uuid": a.ProjectID.String(), "due_at": a.DueAt.UTC().Format(time.RFC3339)}})
	who, ok := hears(ctx, t, "", taskBusiness.MembersOnly)
	if !ok {
		return
	}
	tellAlert(ctx, alertNotice(a, t), []notificationBusiness.NoticeRecipient{{UserUUID: who, Reason: "assignee"}})
}

// alertNotice is a's alert of task t, as its assignee is told. Pure.
func alertNotice(a model.Alert, t *dgraphStruct.DgraphTask) notificationBusiness.Notice {
	name := t.Name
	if name == "" {
		name = "a task"
	}
	kind, title := NoticeAtRisk, name+" is at risk of breaching its SLA"
	if a.Breach {
		kind, title = NoticeBreached, name+" breached its SLA"
	}
	body := ""
	if t.Project != nil {
		body = t.Project.Name
	}
	return notificationBusiness.Notice{
		Kind:          kind,
		ResourceKind:  "task",
		ResourceUUID:  t.Uuid,
		ResourceTitle: t.Name,
		DedupKey:      kind + "|" + t.Uuid + "|" + a.DueAt.UTC().Format(time.RFC3339Nano),
		Title:         title,
		Body:          body,
		Path:          "/app/task/" + t.Uuid,
		Email:         notificationBusiness.EventTaskSLA,
		Push:          &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK, ID: t.Uuid},
	}
}

// open is whether t is a live task still open: one deleted, done or
// cancelled is its timer's end, not an alert.
func open(t *dgraphStruct.DgraphTask) bool {
	return t != nil && !helpers.IsSoftDeleted(t.DeletedAt) &&
		t.Status != dgraphStruct.TASK_STATUS_DONE && t.Status != dgraphStruct.TASK_STATUS_CANCELED
}
