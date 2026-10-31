package business

import (
	"context"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
)

// ReadableTask is a task as someone who may read it sees it named: its name,
// where it stands, and its project.
type ReadableTask struct {
	Name        string
	Status      string
	ProjectUUID string
	ProjectName string
}

// ReadableTasks reads the tasks, of those named, the person (by graph uid)
// may read now, by CanViewTask, keyed by lower-case uuid. A task they may not
// read, deleted, in a deleted project, or not there is left out. One graph
// query for the lot; an error is the graph not answering.
func ReadableTasks(ctx context.Context, taskUUIDs []string, userUID string) (map[string]ReadableTask, error) {
	out := make(map[string]ReadableTask, len(taskUUIDs))
	if len(taskUUIDs) == 0 || userUID == "" {
		return out, nil
	}
	tasks, err := domain.GetTasksReadAccessFor(ctx, taskUUIDs, userUID)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t == nil || t.Uuid == "" || helpers.IsSoftDeleted(t.DeletedAt) ||
			(t.Project != nil && helpers.IsSoftDeleted(t.Project.DeletedAt)) {
			continue
		}
		if CanViewTask(t, userUID) {
			r := ReadableTask{Name: t.Name, Status: t.Status}
			if t.Project != nil {
				r.ProjectUUID, r.ProjectName = t.Project.Uuid, t.Project.Name
			}
			out[strings.ToLower(t.Uuid)] = r
		}
	}
	return out, nil
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
