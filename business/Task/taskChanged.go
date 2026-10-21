package business

import (
	"context"
	"time"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The fields task.changed names. Never renamed: saved rules name them.
const (
	FieldPriority = "priority"
	FieldDue      = "due"
	FieldTags     = "tags"
	FieldName     = "name"
)

// dispatchTaskChanged tells listeners (rules, agents) and webhooks that one
// of a task's fields changed, with what it was and what it is. Status and
// assignee have their own events (task.status_changed, task.assigned). A
// change to the same value says nothing.
func dispatchTaskChanged(ctx context.Context, task *dgraphStruct.DgraphTask, field, before, after string, by *dgraphStruct.DgraphUser) {
	if task == nil || before == after {
		return
	}
	data := map[string]interface{}{
		"task_id":   task.Uuid,
		"task_name": task.Name,
		"field":     field,
		"old":       before,
		"new":       after,
	}
	if task.Project != nil {
		data["project_id"] = task.Project.Uuid
		data["project_name"] = task.Project.Name
	}
	if field == FieldName {
		data["task_name"] = after
	}
	if by != nil {
		data["updated_by_uuid"] = by.Uuid
		data["updated_by_name"] = by.DisplayName()
	}
	helpers.GoSafeNamed("Task.dispatchTaskChanged", func() {
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "task.changed", data)
	})
}

// dayOf is a date as task.changed carries it: RFC 3339, "" when unset.
func dayOf(t *time.Time) string {
	if t == nil || t.Year() <= 1970 {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
