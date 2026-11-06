package business

// One task list filter for every reader of a project's tasks: the list, the
// board and its columns, and the report of a view (B9-b1). Each sends the
// filters the person set, and this turns them into the same graph condition,
// with the side filters (cycles, custom fields, types) read from Postgres.

import (
	"context"
	"strings"
	"time"

	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// ListFilter is a project's task list filters (params, and the search box's
// search) as of now. Without a project it has no side filters, as a
// person's own list has none: a cycle, a field or a type is a project's.
func ListFilter(ctx context.Context, projectUUID string, params []taskDomain.TaskListParam, search string, now time.Time) taskDomain.TaskListFilter {
	f := taskDomain.TaskListFilter{Params: params, Search: search, Now: now, Status: taskStatusBusiness.FilterClause}
	if projectUUID != "" {
		f.Side = func(id string, values []string) (string, bool) {
			return sideFilterClause(ctx, projectUUID, id, values)
		}
	}
	return f
}

// sideFilterClause is the clause for a filter on what Postgres keeps beside a
// project's tasks: their cycle (business/Cycle) or a custom field's value
// (business/TaskField). ok is false for any other filter.
func sideFilterClause(ctx context.Context, projectUUID, id string, values []string) (clause string, ok bool) {
	var err error
	switch {
	case id == "task_cycle":
		clause, err = cycleBusiness.FilterClause(projectUUID, values)
	case strings.HasPrefix(id, taskFieldBusiness.FilterPrefix):
		var project uuid.UUID
		if project, err = uuid.Parse(projectUUID); err == nil {
			clause, err = taskFieldBusiness.FilterClause(ctx, project, id, values)
		}
	case id == workTaxonomy.FilterID:
		var project uuid.UUID
		if project, err = uuid.Parse(projectUUID); err == nil {
			clause, err = workTaxonomy.FilterClause(ctx, project, values)
		}
	default:
		return "", false
	}
	// A filter that couldn't be read narrows to nothing: an empty list says
	// something went wrong, where the whole list would pass for a filtered one.
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Project %s filter err: %+v", id, err)
		return `eq(task_uuid, "00000000-0000-0000-0000-000000000000")`, true
	}
	return clause, true
}
