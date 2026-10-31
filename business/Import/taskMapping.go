package business

import (
	"context"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/google/uuid"
)

// The mapping every provider's tasks and sub-tasks go through (I1), once.

// taskLabel is a source task's labels as one task's tags: all of them (the
// first was all that was kept), as a person's are stored.
func taskLabel(labels []string) string {
	return helpers.NormaliseTags(strings.Join(labels, ","))
}

// closedIfCompleted is a task's mapped status, closed (done) when the source
// says it is completed and the status it mapped to isn't closed: a task the
// source finished stays finished whatever its column was called.
func closedIfCompleted(mapped string, completed bool) string {
	if completed && mapped != dgraphStruct.TASK_STATUS_DONE && mapped != dgraphStruct.TASK_STATUS_CANCELED {
		return dgraphStruct.TASK_STATUS_DONE
	}
	return mapped
}

// sourceNumber is the number a task had in the source (ENG-123's 123), which
// it keeps when its project hasn't used it; 0 for none.
func sourceNumber(st *importProvider.SourceTask) int {
	_, n := importProvider.SplitKey(st.SourceKey)
	return n
}

// builtinType is the WorkTaxonomy type a source's task comes as; nil is a
// Task.
func builtinType(t importProvider.TaskType) *uuid.UUID {
	if t != importProvider.TaskMilestone {
		return nil
	}
	id := workTaxonomy.TypeMilestone
	return &id
}

// coAssigneeNames are the names of a source task's assignees after the first
// (who is its assignee), for the line its description keeps. Each person is
// read once a chunk, through who.
func coAssigneeNames(ctx context.Context, who *people, ids []string) []string {
	names := []string{}
	for _, id := range ids {
		if _, info := who.resolve(ctx, id); info != nil {
			names = append(names, info.UserDgraphInfo.DisplayName())
		}
	}
	return names
}

// keepSourceKey gives a project the source's own key (Jira's ENG) when no
// other project has or had it, so its tasks' keys read as they did; one taken
// is said in the import's log, and the project keeps the key made from its name.
func keepSourceKey(ctx context.Context, job *importModels.Job, sp *importProvider.SourceProject, project, by uuid.UUID) {
	if sp.SourceKey == "" {
		return
	}
	if _, _, err := taskKeys.SetKey(ctx, project, sp.SourceKey, by); err != nil {
		importModels.LogImportError(ctx, job.Id, nil, importModels.EntityProject, sp.SourceID,
			importModels.SeverityWarning, "KEY_NOT_KEPT", "Its key "+sp.SourceKey+" couldn't be kept: "+err.Error(), nil)
	}
}

// keepNumbersFree has a project number the tasks an import makes with no
// number of their own (Linear's milestones) after every number its source's
// tasks keep. Sub-tasks come in later chunks than their project's tasks, so
// one would otherwise find its number taken, and its key naming another task.
func keepNumbersFree(ctx context.Context, sp *importProvider.SourceProject, project uuid.UUID) {
	if sp.HighestNumber < 1 {
		return
	}
	if err := taskKeyModel.NumberAfter(ctx, project, sp.HighestNumber); err != nil {
		helpers.LogWarnWithContext(ctx, "import: project %s couldn't keep numbers up to %d for its source's tasks: %+v",
			project, sp.HighestNumber, err)
	}
}

// estimateMinutes is a source's estimate as a task holds it: at most a
// thousand hours, and none for none or a negative one.
func estimateMinutes(m int) int {
	if m <= 0 {
		return 0
	}
	return min(m, taskBusiness.MaxEstimateMinutes)
}

// keepTaskAliases is taskKeys.KeepSourceKeys (a seam for tests).
var keepTaskAliases = taskKeys.KeepSourceKeys

// keepSourceAliases makes the keys a task had in its source find it beside its
// own (O-L2): Linear's ENG-7 when its project here is keyed MOB, and the keys
// it had before it moved teams. A key a project here uses, or one another task
// already answers to, stays as it is, and the import says so once for the
// task. A key no task key could be (Jira's MY_APP-7) is left out quietly: the
// project's KEY_NOT_KEPT said so.
func keepSourceAliases(ctx context.Context, job *importModels.Job, st *importProvider.SourceTask, task uuid.UUID) {
	raws := append([]string{st.SourceKey}, st.FormerKeys...)
	held, err := keepTaskAliases(ctx, raws, task, job.Id)
	msg := ""
	switch {
	case err != nil:
		msg = "Its keys from " + job.Provider + " couldn't be kept: " + err.Error()
	case len(held) > 0:
		msg = strings.Join(held, ", ") + " already name something else here, so they open that, not this task."
	default:
		return
	}
	importModels.LogImportError(ctx, job.Id, nil, importModels.EntityTask, st.SourceID,
		importModels.SeverityWarning, "KEY_ALIAS_NOT_KEPT", msg, nil)
}
