package business

// A to-do in a doc and the task it became (docs programme C1): ⌘⇧Enter on a
// to-do makes a task and ties the two, and from then on either side moves the
// other.
//
//   - The doc to the task: a tick in the doc changes the task through
//     SetTodoDone, the ONE place the doc writes to a task. It goes through
//     today's task write path (UpdateTaskStatusByTaskUUID) with the task view's
//     own rule, so nobody changes a task from a doc that they couldn't change
//     from its panel. The projects programme's Task Writer (A3) replaces the
//     call here.
//   - The task to the doc: a task done or opened again anywhere (its board,
//     its panel, the API, an agent) ticks or clears its to-do in every doc that
//     holds it, through the live doc (collab.SetTaskChecks), credited to whoever
//     changed the task. It listens for task.status_changed, so no task write
//     path knows about docs.
//
// Which docs hold which task is doc_task_links (migration 340): a row only the
// conversion writes. It goes when a change to the task finds the doc no longer
// holds the to-do, not when a save drops it: a to-do deleted and brought back
// with undo, or cut and pasted, keeps following its task.

import (
	"context"
	"errors"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	linkModels "github.com/akashc777/OneCamp/models/postgres/EntityLink"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// Refusals a person can act on.
var (
	// ErrDocNotEditable is a doc the person can't edit, so its to-dos are not theirs to change.
	ErrDocNotEditable = errors.New("you can't edit this doc")
	// ErrTodoNotTied is a to-do that doesn't stand for that task in that doc.
	ErrTodoNotTied = errors.New("that to-do isn't a task in this doc")
	// ErrEmptyTodo is a to-do with nothing written in it yet.
	ErrEmptyTodo = errors.New("the to-do is empty")
)

// Seams for tests: where the ties are kept, the live doc, and the doc check.
var (
	tieDocTask   = linkModels.AddDocTask
	untieDocTask = linkModels.RemoveDocTask
	docHoldsTask = linkModels.HasDocTask
	docsHolding  = linkModels.DocsForTask
	setChecks    = func(ctx context.Context, docUUID string, checks map[string]bool, o collab.Options) (collab.TaskChecks, error) {
		return collab.SetTaskChecks(ctx, docUUID, checks, o)
	}
	docLevel = func(ctx context.Context, user *model.UserInfo, docUUID string) (accessBusiness.Level, error) {
		d, err := accessBusiness.Rules.Resolve(ctx, accessBusiness.SubjectOf(user),
			accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
		if errors.Is(err, accessBusiness.ErrNotFound) {
			return accessBusiness.LevelNone, nil
		}
		return d.Level, err
	}
	readProject        = projectDomain.GetBasicDgraphProjectInfo
	readTask           = GetDgraphBasicTaskInfo
	writeStatus        = UpdateTaskStatusByTaskUUID
	makeTask           = CreateTask
	publishTaskChanged = mqttBusiness.PublishTaskChanged
)

func init() {
	webhookBusiness.RegisterEventListener(taskToDocs)
}

func mayEditDoc(ctx context.Context, user *model.UserInfo, docUUID string) error {
	level, err := docLevel(ctx, user, docUUID)
	if err != nil {
		return err
	}
	if level < accessBusiness.LevelEdit {
		return ErrDocNotEditable
	}
	return nil
}

// TodoToTask makes a task named title in a project from a to-do in a doc, and
// ties the two. The person must be able to edit the doc and, as in "Add task",
// make tasks in the project (Work Access).
func TodoToTask(ctx context.Context, user *model.UserInfo, docUUID, projectUUID, title string) (uuid.UUID, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return uuid.Nil, ErrEmptyTodo
	}
	doc, err := uuid.Parse(docUUID)
	if err != nil {
		return uuid.Nil, err
	}
	project, err := uuid.Parse(projectUUID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := mayEditDoc(ctx, user, doc.String()); err != nil {
		return uuid.Nil, err
	}
	p, err := readProject(ctx, project.String(), user.UserDgraphInfo.Uid)
	if err != nil {
		return uuid.Nil, err
	}
	if !p.ReaderIsIn() {
		return uuid.Nil, ErrTaskNotVisible
	}
	s, err := workAccess.Of(ctx, user.UserDgraphInfo.Uuid, p)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.Check(workAccess.NeedCreateTask); err != nil {
		return uuid.Nil, err
	}
	// As "Add task" makes it: medium priority, in To do.
	in := adapter.CreateOrUpdateTaskInput{TaskName: title, ProjectUuid: project.String(), Priority: dgraphStruct.TASK_PRIORITY_MEDIUM}
	task, err := makeTask(ctx, project, user, p, nil, in, nil)
	if err != nil {
		return uuid.Nil, err
	}
	by, _ := uuid.Parse(user.UserDgraphInfo.Uuid)
	if err := tieDocTask(ctx, doc, task, by); err != nil {
		// The task is made; its to-do just won't tick from elsewhere. Said, not hidden.
		helpers.LogErrorWithContext(ctx, "business/TodoToTask doc %s task %s: the tie wasn't kept, so the to-do won't follow the task: %+v", doc, task, err)
	}
	return task, nil
}

// SetTodoDone is a tick (done) or an untick in a doc, made to the task its
// to-do stands for: done, or back to To do.
func SetTodoDone(ctx context.Context, user *model.UserInfo, docUUID, taskUUID string, done bool) error {
	doc, err := uuid.Parse(docUUID)
	if err != nil {
		return err
	}
	task, err := uuid.Parse(taskUUID)
	if err != nil {
		return err
	}
	tied, err := docHoldsTask(ctx, doc, task)
	if err != nil {
		return err
	}
	if !tied {
		return ErrTodoNotTied
	}
	if err := mayEditDoc(ctx, user, doc.String()); err != nil {
		return err
	}
	info, err := readTask(ctx, task.String(), user.UserDgraphInfo.Uid)
	if err != nil {
		return err
	}
	if info == nil || !CanViewTask(info, user.UserDgraphInfo.Uid) {
		return ErrTaskNotVisible
	}
	s, f, err := workAccess.OfTask(ctx, user.UserDgraphInfo.Uuid, user.UserDgraphInfo.Uid, info)
	if err != nil {
		return err
	}
	if err := s.CheckTask(workAccess.NeedEditTask, f); err != nil {
		return err
	}
	status := dgraphStruct.TASK_STATUS_TODO
	if done {
		status = dgraphStruct.TASK_STATUS_DONE
	}
	return writeStatus(ctx, task, status, info, &user.UserDgraphInfo)
}

// isDone is whether a status category is a ticked box.
func isDone(category string) bool { return category == dgraphStruct.TASK_STATUS_DONE }

// taskChanges are the events after which a task reads differently on a chip:
// its status or assignee, or whether it is there at all.
var taskChanges = map[string]bool{
	"task.status_changed": true, "task.assigned": true, "task.created": true, "task.deleted": true, "task.restored": true,
}

// taskRenamed tells a renamed task's project topic, so chips and task lists
// in docs read its name again: a rename dispatches no event (it isn't one of
// the webhooks'), so taskToDocs never hears of it.
func taskRenamed(task uuid.UUID, project *dgraphStruct.DgraphProject) {
	if project == nil || project.Uuid == "" {
		return
	}
	publishTaskChanged(&mqttStruct.MqttTaskChanged{TaskUuid: task.String(), ProjectUuid: project.Uuid})
}

// taskToDocs is the listener. Any change a chip shows is told on the task's
// project's topic, so open chips and task lists read it again; and a task
// that became done, or stopped being done, ticks or clears its to-do in every
// doc that holds it. A doc that no longer holds the to-do, or is gone for
// good, stops following the task; one in the trash keeps it, for a restore.
func taskToDocs(ctx context.Context, eventType string, data map[string]interface{}) {
	if !taskChanges[eventType] {
		return
	}
	str := func(k string) string { s, _ := data[k].(string); return s }
	publishTaskChanged(&mqttStruct.MqttTaskChanged{TaskUuid: str("task_id"), ProjectUuid: str("project_id")})
	if eventType != "task.status_changed" {
		return
	}
	move := taskStatusBusiness.MoveFromEvent(data)
	done := isDone(move.NewStatus)
	if done == isDone(move.OldStatus) {
		return
	}
	task, err := uuid.Parse(str("task_id"))
	if err != nil {
		return
	}
	docs, err := docsHolding(ctx, task)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/taskToDocs task %s: its docs couldn't be read, so their to-dos weren't ticked: %+v", task, err)
		return
	}
	for _, doc := range docs {
		got, err := setChecks(ctx, doc.String(), map[string]bool{task.String(): done}, collab.Options{By: collab.By{UserUUID: str("updated_by_uuid")}})
		switch {
		case errors.Is(err, collab.ErrDocInTrash):
			// Kept for a restore.
		case errors.Is(err, collab.ErrNoSuchDoc), err == nil && got.Held == 0:
			if err := untieDocTask(ctx, doc, task); err != nil {
				helpers.LogErrorWithContext(ctx, "business/taskToDocs task %s doc %s: the doc no longer holds its to-do, but still follows it: %+v", task, doc, err)
			}
		case err != nil:
			helpers.LogErrorWithContext(ctx, "business/taskToDocs task %s doc %s: its to-do wasn't ticked: %+v", task, doc, err)
		}
	}
}
