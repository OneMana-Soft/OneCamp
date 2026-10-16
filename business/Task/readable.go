package business

import (
	"context"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
)

// ReadableTaskTitles names the tasks, of those named, the person (by graph
// uid) may read now, by CanViewTask: each one's name, keyed by lower-case
// uuid. A task they may not read, deleted, in a deleted project, or not there
// is left out. One graph query for the lot; an error is the graph not
// answering.
func ReadableTaskTitles(ctx context.Context, taskUUIDs []string, userUID string) (map[string]string, error) {
	out := make(map[string]string, len(taskUUIDs))
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
			out[strings.ToLower(t.Uuid)] = t.Name
		}
	}
	return out, nil
}
