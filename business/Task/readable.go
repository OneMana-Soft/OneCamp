package business

import (
	"context"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ReadableTask is a task as someone who may read it sees it named: its name,
// where it stands, and its project. Read for a calendar block
// (ReadableBlockTasks), also how many open tasks it waits on in the reader's
// projects and the first of them when the reader may read that one too.
type ReadableTask struct {
	Name         string
	Status       string
	ProjectUUID  string
	ProjectName  string
	BlockedOpen  int
	FirstBlocker *ReadableRef
}

// ReadableRef names a task the reader may read: its uuid (lower case) and name.
type ReadableRef struct {
	UUID string
	Name string
}

// ReadableTasks is the tasks, of those named, the person (by graph uid) may
// read now, keyed by lower-case uuid: one read, judged by CanViewTask.
func ReadableTasks(ctx context.Context, taskUUIDs []string, userUID string) (map[string]ReadableTask, error) {
	if len(taskUUIDs) == 0 || userUID == "" {
		return map[string]ReadableTask{}, nil
	}
	tasks, err := domain.GetTasksReadAccessFor(ctx, taskUUIDs, userUID)
	if err != nil {
		return nil, err
	}
	return readableOf(tasks, userUID), nil
}

// ReadableBlockTasks is ReadableTasks for calendar blocks (CAL51-TB), read for
// the person with graph uid userUID and uuid userUUID: each task also says
// what it waits on. Only open blockers in the reader's projects count, and
// the first is named only when the reader may read it (CanViewTask, as any
// task); anyone else gets the count alone. One read.
func ReadableBlockTasks(ctx context.Context, taskUUIDs []string, userUID, userUUID string) (map[string]ReadableTask, error) {
	if len(taskUUIDs) == 0 || userUID == "" {
		return map[string]ReadableTask{}, nil
	}
	tasks, err := domain.GetBlockTasksFor(ctx, taskUUIDs, userUID, userUUID)
	if err != nil {
		return nil, err
	}
	return readableOf(tasks, userUID), nil
}

// standing is a task live (taskDependency.go) in a project not deleted.
func standing(t *dgraphStruct.DgraphTask) bool {
	return live(t) && (t.Project == nil || !helpers.IsSoftDeleted(t.Project.DeletedAt))
}

// readableOf is the tasks the person may read, as ReadableTasks answers. Pure.
func readableOf(tasks []*dgraphStruct.DgraphTask, userUID string) map[string]ReadableTask {
	out := make(map[string]ReadableTask, len(tasks))
	for _, t := range tasks {
		if !standing(t) || !CanViewTask(t, userUID) {
			continue
		}
		r := ReadableTask{Name: t.Name, Status: t.Status, BlockedOpen: int(t.BlockedOpen)}
		if t.Project != nil {
			r.ProjectUUID, r.ProjectName = t.Project.Uuid, t.Project.Name
		}
		if len(t.BlockedBy) > 0 {
			if b := t.BlockedBy[0]; standing(b) && CanViewTask(b, userUID) {
				r.FirstBlocker = &ReadableRef{UUID: strings.ToLower(b.Uuid), Name: b.Name}
			}
		}
		out[strings.ToLower(t.Uuid)] = r
	}
	return out
}

// ReadableTaskTitles names the tasks, of those named, the person may read
// now (ReadableTasks): each one's name, keyed by lower-case uuid.
func ReadableTaskTitles(ctx context.Context, taskUUIDs []string, userUID string) (map[string]string, error) {
	tasks, err := ReadableTasks(ctx, taskUUIDs, userUID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tasks))
	for id, t := range tasks {
		out[id] = t.Name
	}
	return out, nil
}
