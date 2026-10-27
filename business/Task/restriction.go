package business

import (
	"context"
	"time"

	"github.com/google/uuid"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Changer is who changes who may see a task: the person it is restricted by
// (By, users.id), their profile's uuid (UserUUID, the event's actor), and how
// the change is recorded in the admin audit log: a person's request
// (auditBusiness.Record), or the workspace acting for someone, as an old
// import's re-check does for the person whose connection read the source.
type Changer struct {
	By       uuid.UUID
	UserUUID string
	Record   func(action, summary string, meta map[string]any)
}

// Seams for tests.
var (
	restrictTask = workAccess.Restrict
	openTask     = workAccess.Unrestrict
)

// SetRestriction keeps task to its project's admins, its assignee, its
// creator and people (restrict: WorkAccess.Restrict, replacing whoever was let
// in before), or opens it to its project again, and answers whether that
// changed it. A change is recorded (CategorySecurity: the task, its project
// and how many were let in, never who) and told to realtime and webhooks
// (TaskChanged). The caller has checked that who may (WorkAccess
// MayRestrict), and words the refusal: the app's route
// (controllers/TaskRestriction) and an old import's re-check
// (business/Import) both change it here.
func SetRestriction(ctx context.Context, task *dgraphStruct.DgraphTask, restrict bool, people []uuid.UUID, who Changer, at time.Time) (bool, error) {
	changed := false
	var err error
	if restrict {
		err = restrictTask(ctx, task, who.By, people, at)
		changed = err == nil
	} else {
		changed, err = openTask(ctx, task)
	}
	if err != nil || !changed {
		return false, err
	}
	project := ""
	if task.Project != nil {
		project = task.Project.Uuid
	}
	action, summary, word := "task.restricted", "Kept a task to a few people", "true"
	if !restrict {
		action, summary, word = "task.unrestricted", "Opened a restricted task to its project", "false"
	}
	who.Record(action, summary, map[string]any{"task_uuid": task.Uuid, "project_uuid": project, "people": len(people)})
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.TaskChanged, ResourceKind: workAccess.KindTask, ResourceUUID: task.Uuid,
		Actor: resourceEventBusiness.Actor{UserUUID: who.UserUUID}, At: at,
		Data: map[string]string{"restricted": word},
	})
	return true, nil
}
