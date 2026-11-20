package business

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// assigneeWrite is the one write of a task's assignee: to (nil: nobody) in
// place of from, by by at at, as the graph has it (the edge, and an activity
// from whom to whom) and as search has it, with the task's deletion time
// written as deletedAt: the zero time for a live task, so a late write never
// leaves one deleted (UpdateTaskAssigneeByTaskUUID refuses a deleted one), or
// the time it went to the trash, where it stays (AssignInTrash). by nil (or
// with no node) is nobody here: the activity names no one. Pure.
func assigneeWrite(taskUUID string, from, to, by *dgraphStruct.DgraphUser, at, deletedAt time.Time) (*dgraphStruct.DgraphTask, *openSearchStruct.OpenSearchTask) {
	activity := &dgraphStruct.DgraphTaskActivity{Uuid: uuid.NewString(), Type: dgraphStruct.ACTIVITY_TYPE_ASSIGNEE, LogTime: &at}
	if by != nil && by.Uid != "" {
		activity.CreatedBy = &dgraphStruct.DgraphUser{Uid: by.Uid}
	}
	write := &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: taskUUID, UpdatedAt: &at, DeletedAt: &deletedAt,
		Activity: []*dgraphStruct.DgraphTaskActivity{activity}}
	none := ""
	search := &openSearchStruct.OpenSearchTask{Uuid: taskUUID, TaskUpdatedAt: at.Unix(), TaskAssigneeUuid: &none}
	if !deletedAt.IsZero() {
		trashedAt := deletedAt.Unix()
		search.TaskDeletedAt = &trashedAt
	}
	if from != nil {
		activity.PrevState = from.DisplayName()
	}
	if to != nil {
		write.Assignee = &dgraphStruct.DgraphUser{Uid: to.Uid, Tasks: []*dgraphStruct.DgraphTask{{Uid: "uid(task)"}}}
		activity.NextState = to.DisplayName()
		search.TaskAssigneeUuid, search.TaskAssigneeFullName = &to.Uuid, to.DisplayName()
	}
	return write, search
}

// AssignInTrash hands a task in the trash to to (nil takes its assignee off)
// and leaves it there, by by: the graph's edge and the search entry's
// assignee, with its deletion time as it was, so a restore brings it back as
// left. An old import's re-check takes off an assignee its source doesn't let
// in, and an admin's undo of it puts them back, whatever the task's state
// (business/Import recheck.go). Handed only to someone who can be given work,
// as every path that assigns.
func AssignInTrash(ctx context.Context, task *dgraphStruct.DgraphTask, to, by *dgraphStruct.DgraphUser) error {
	if to != nil {
		if err := principalBusiness.Unassignable(to, principalBusiness.Assigned); err != nil {
			return err
		}
	}
	from := ""
	if task.Assignee != nil {
		from = task.Assignee.Uid
	}
	write, search := assigneeWrite(task.Uuid, task.Assignee, to, by, time.Now(), *task.DeletedAt)
	if _, err := domain.UpdateDgraphTaskAssignee(ctx, write, from, task.Uid); err != nil {
		return err
	}
	domain.UpdateTaskInOpenSearch(search)
	if userUUIDOf(task.Assignee) != userUUIDOf(to) {
		aiAssignee(ctx, task.Uuid, userUUIDOf(to))
		logChange(ctx, by, workFeed.TaskChange{Kind: workFeed.KindUpdated, Project: projectOf(task),
			Task: &dgraphStruct.DgraphTask{Uuid: task.Uuid, Assignee: to}, Fields: []string{"task_assignee"}})
	}
	return nil
}

// aiAssigneeWrite writes hand-overs (task uuid -> its assignee now, "" for
// nobody) onto the tasks' AI entries in one write: assigneeAI.go in the AI
// edition, nil without it.
var aiAssigneeWrite func(ctx context.Context, to map[string]string) error

// startAIAssigneeBackfill starts the AI edition's startup repair of the
// tasks' AI entries (assigneeAIBackfill.go); nil without it.
var startAIAssigneeBackfill func(context.Context)

// StartAIAssigneeBackfill starts it where there is one: once per install, in
// the background, every task's assignee now written onto its AI entry.
func StartAIAssigneeBackfill(ctx context.Context) {
	if startAIAssigneeBackfill != nil {
		startAIAssigneeBackfill(ctx)
	}
}

// aiAssignee writes a hand-over onto the task's AI entry (aiAssigneeWrite),
// so Ask AI finds the task for its new assignee from outside its project.
// Inside a bulk run (HandOvers) it is kept for the run's one write instead. A
// write that fails is logged, and written once more in the background with
// the task's assignee as it is then (assigneeAI.go): every AI search checks
// that grant against the task as it is (services/AI keepAssigned), so the
// entry can't give the task to someone it has gone from. Should the retry
// fail too, its new assignee from outside the project misses the task in Ask
// AI (a miss, never a leak).
func aiAssignee(ctx context.Context, taskUUID, to string) {
	if aiAssigneeWrite == nil {
		return
	}
	if h, ok := ctx.Value(handOversKey{}).(*handOvers); ok {
		h.add(taskUUID, to)
		return
	}
	writeAIAssignees(ctx, map[string]string{taskUUID: to})
}

func writeAIAssignees(ctx context.Context, to map[string]string) {
	if len(to) == 0 {
		return
	}
	if err := aiAssigneeWrite(ctx, to); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Task: %d tasks' new assignees aren't on their AI entries: %v", len(to), err)
	}
}

type handOversKey struct{}

// handOvers is a bulk run's hand-overs, by task: a task handed over twice in
// the run keeps the last.
type handOvers struct {
	mu sync.Mutex
	to map[string]string
}

func (h *handOvers) add(taskUUID, to string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.to[taskUUID] = to
}

// HandOvers is for a run that hands over many tasks (a page of an import's
// re-check, its undo, a page of the assignee repair): the hand-overs made
// with the ctx it answers keep their AI writes back, and write sends them
// all in one write, which the run calls when it is done, failed or not (what
// was handed over is in the graph already). One task alone writes at once.
func HandOvers(ctx context.Context) (context.Context, func(ctx context.Context)) {
	h := &handOvers{to: map[string]string{}}
	return context.WithValue(ctx, handOversKey{}, h), func(ctx context.Context) {
		h.mu.Lock()
		to := h.to
		h.to = map[string]string{}
		h.mu.Unlock()
		writeAIAssignees(ctx, to)
	}
}
