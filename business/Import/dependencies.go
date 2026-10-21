package business

import (
	"context"
	"encoding/json"
	"errors"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Dependencies (I1): a source's "blocked by" links come across as the task's
// dependencies, with their kind and lag (business/Task AddTaskDependency, the
// one way a person adds one). A task keeps what it waits on with its mapping
// (taskMapMetadata), and once every task and subtask has landed this stage
// links them: the blocker may be anywhere in the import.

// taskMapMetadata is what a task's mapping keeps of it: its name, project,
// author and mapped status and priority, and what it waits on (each blocker
// once, never itself).
func taskMapMetadata(st *importProvider.SourceTask, status, priority string) map[string]any {
	md := map[string]any{
		"name":              st.Name,
		"project_source_id": st.ProjectSourceID,
		"author":            st.CreatedBy,
		"final_status":      status,
		"final_priority":    priority,
	}
	seen := map[string]bool{st.SourceID: true}
	var deps []importProvider.SourceDependency
	for _, d := range st.Dependencies {
		if d.BlockerSourceID == "" || seen[d.BlockerSourceID] {
			continue
		}
		seen[d.BlockerSourceID] = true
		if d.Kind == "" {
			d.Kind = taskBusiness.FinishToStart
		}
		deps = append(deps, d)
	}
	if len(deps) > 0 {
		md["blocked_by"] = deps
	}
	return md
}

// runDependenciesStage links every imported task to what it waits on. A
// blocker that didn't come across as a task (a sub-task is not one; OneCamp's
// dependencies are between tasks), or a link OneCamp doesn't keep (across
// projects, a loop), is said in the import's log and the rest go on.
func runDependenciesStage(ctx context.Context, job *importModels.Job, user *userModels.UserInfo) error {
	waiting, err := importModels.IdMappingsWithMetadata(ctx, job.Id, importModels.EntityTask, "blocked_by")
	if err != nil {
		return err
	}
	if len(waiting) == 0 {
		return nil
	}
	// Where every task landed, read once: a blocker may be anywhere in the import.
	landed, err := importModels.IdMapOf(ctx, job.Id, importModels.EntityTask)
	if err != nil {
		return err
	}
	me := user.UserDgraphInfo
	for _, w := range waiting {
		var deps []importProvider.SourceDependency
		if err := json.Unmarshal(w.Value, &deps); err != nil {
			continue
		}
		for _, d := range deps {
			blocker, ok := landed[d.BlockerSourceID]
			if !ok || blocker == uuid.Nil {
				importModels.LogImportError(ctx, job.Id, nil, importModels.EntityTask, w.SourceId,
					importModels.SeverityWarning, "DEPENDENCY_UNRESOLVED", "It waits on "+d.BlockerSourceID+", which wasn't imported.", nil)
				continue
			}
			err := taskBusiness.AddTaskDependency(ctx, w.OnecampUUID.String(), blocker.String(), d.Kind, d.LagDays, &me)
			if err != nil {
				importModels.LogImportError(ctx, job.Id, nil, importModels.EntityTask, w.SourceId,
					importModels.SeverityWarning, "DEPENDENCY_NOT_KEPT", dependencyWords(err, d.BlockerSourceID), nil)
			}
		}
	}
	return nil
}

// dependencyWords says why a dependency wasn't kept.
func dependencyWords(err error, blocker string) string {
	switch {
	case errors.Is(err, taskBusiness.ErrDependencySubtask):
		return "Its wait on " + blocker + " involves a sub-task, which OneCamp's dependencies don't."
	case errors.Is(err, taskBusiness.ErrDependencyProject):
		return "It waits on " + blocker + " in another project."
	case errors.Is(err, taskBusiness.ErrDependencyLoop):
		return "Its wait on " + blocker + " would make a loop."
	}
	return "Its wait on " + blocker + " couldn't be kept: " + err.Error()
}
