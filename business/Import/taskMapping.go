package business

import (
	"context"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
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

// estimateMinutes is a source's estimate as a task holds it: at most a
// thousand hours, and none for none or a negative one.
func estimateMinutes(m int) int {
	if m <= 0 {
		return 0
	}
	return min(m, taskBusiness.MaxEstimateMinutes)
}
