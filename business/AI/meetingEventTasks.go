package business

// "Make task" from an action item of an event's call's recap
// (gcal-new-event-notes, A2). One server call makes the task through the task
// path every proposed task takes (createProposedTask: its owner resolved to a
// member where one matches, its due date set) in a project the person may add
// tasks to (projectToAddTasksTo, Work Access), notes it on the item, and puts
// it on the event through the event's own files (business/Calendar AddFile,
// EVLINK: ref_kind task), under that path's own rule. Nothing is copied: the
// item names the task, and the event carries it by reference.
//
// WHO. Whoever may change the event (its organiser, or a guest the organiser
// lets change it: CallArtefactKeysToChange, the rule that links a task to it)
// and may add tasks to the project the task goes in. A guest who may only
// read the event is refused as for any change; anyone else is told there is
// no such event. An item makes one task: a second ask answers the first's,
// and two at once wait on one claim (AIActionTaskClaim).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

func init() { calendarBusiness.OnMakeActionTask(MakeEventActionTask) }

// The reads and writes MakeEventActionTask goes through (seams for tests).
var (
	eventKeysToChange = func(ctx context.Context, who *userModels.UserInfo, id string) (calendarBusiness.CallKeys, error) {
		return calendarBusiness.Events.CallArtefactKeysToChange(ctx, who, id)
	}
	eventCallOf     = callStateBusiness.EventArtefact
	projectForTask  = projectToAddTasksTo
	makeActionTask  = createProposedTask
	noteActionTask  = recordingDomain.RecordActionItemTask
	linkTaskToEvent = func(ctx context.Context, who *userModels.UserInfo, id, task string) error {
		_, err := calendarBusiness.Events.AddFile(ctx, who, id, calendarBusiness.FileRef{Kind: entityLinkBusiness.RefTask, UUID: task})
		return err
	}
	claimActionItem = func(ctx context.Context, key string, i int, token string) (bool, error) {
		return redisStore.SetStringIfAbsent(ctx, registry.AIActionTaskClaim, []string{key, strconv.Itoa(i)}, token, registry.AIActionTaskClaim.TTL)
	}
	releaseActionItem = func(ctx context.Context, key string, i int, token string) {
		_, _ = redisStore.DeleteIfHeld(ctx, registry.AIActionTaskClaim, []string{key, strconv.Itoa(i)}, token)
	}
)

// MakeEventActionTask makes the task of action item i of the call with this
// key on the event id names (an instance's calls are its series'), in the
// project projectUUID names, AS who, and puts it on the event: the calendar's
// maker (business/Calendar MakeActionTask). The item's task when it has one
// already (Made false). The calendar's refusals as they are,
// calendarBusiness.ErrCantAddTasks (wrapped, saying why), ErrNoActionItem or
// ErrActionItemBusy.
func MakeEventActionTask(ctx context.Context, who *userModels.UserInfo, id, key string, i int, projectUUID string) (*calendarBusiness.ActionTask, error) {
	keys, err := eventKeysToChange(ctx, who, id)
	if err != nil {
		return nil, err
	}
	item, err := actionItemOf(ctx, keys, key, i)
	if err != nil || item.Task != "" {
		return madeAlready(item), err
	}
	projectID, project, err := projectForTask(ctx, who, projectUUID)
	var refused *ProjectRefusal
	if errors.As(err, &refused) {
		return nil, fmt.Errorf("%w: %s", calendarBusiness.ErrCantAddTasks, refused.Error())
	}
	if err != nil {
		return nil, err
	}
	token := uuid.NewString()
	won, err := claimActionItem(ctx, key, i, token)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI MakeEventActionTask: the claim on %s item %d failed, going on: %v", key, i, err)
	}
	if !won {
		return nil, calendarBusiness.ErrActionItemBusy
	}
	defer releaseActionItem(context.WithoutCancel(ctx), key, i, token)
	// Read again under the claim: a task made just before it is the item's.
	if item, err = actionItemOf(ctx, keys, key, i); err != nil || item.Task != "" {
		return madeAlready(item), err
	}
	t := ProposedTask{Title: cleanLabel(item.Text, extractMaxTitleLen), AssigneeName: item.Owner, Due: normalizeDueDate(item.Due)}
	description := ""
	if strings.TrimSpace(item.Text) != t.Title {
		description = strings.TrimSpace(item.Text)
	}
	task, err := makeActionTask(ctx, who, projectID, project, t, description)
	if err != nil {
		return nil, err
	}
	out := &calendarBusiness.ActionTask{TaskUUID: task.String(), Title: t.Title, Made: true}
	if err := noteActionTask(ctx, key, i, task); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI MakeEventActionTask: task %s made from %s item %d, not noted on it: %v", task, key, i, err)
	}
	if err := linkTaskToEvent(ctx, who, id, task.String()); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI MakeEventActionTask: task %s made, not put on event %s: %v", task, id, err)
		return out, nil
	}
	out.Linked = true
	return out, nil
}

// actionItemOf is item i of the call with this key filed under the event's
// calls (keys): calendarBusiness.ErrNoActionItem for no such call or item.
func actionItemOf(ctx context.Context, keys calendarBusiness.CallKeys, key string, i int) (recordingDomain.ActionItem, error) {
	call, err := eventCallOf(ctx, keys, key)
	if err != nil {
		return recordingDomain.ActionItem{}, err
	}
	if call == nil || i < 0 || i >= len(call.ActionItems) {
		return recordingDomain.ActionItem{}, calendarBusiness.ErrNoActionItem
	}
	return call.ActionItems[i], nil
}

// madeAlready is the task an item has, as making it again answers it; nil
// for an item with none. Pure.
func madeAlready(item recordingDomain.ActionItem) *calendarBusiness.ActionTask {
	if item.Task == "" {
		return nil
	}
	return &calendarBusiness.ActionTask{TaskUUID: item.Task, Title: cleanLabel(item.Text, extractMaxTitleLen)}
}
