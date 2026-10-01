package business

import (
	"context"
	"fmt"
	"strings"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	authService "github.com/akashc777/OneCamp/services/Auth"
)

// The task_status_changed trigger: "when a task moves to QA, post in
// #engineering". Its trigger config says which moves count:
//
//	{"project_id": "<uuid>", "to_status": "<built-in key or custom status id>"}
//
// Both are optional. A built-in status fires when a task's category becomes
// it, so "done" covers a project's own statuses that count as Done. A custom
// status fires when a task enters that status. Replies post in the workflow's
// channel, since a task has none of its own.

// normalizeTaskStatusTrigger checks and rewrites a task_status_changed
// workflow's config before it is saved (see TaskStatus.NormalizeMoveFilter),
// and a reply must have a channel to post in. Other triggers pass through.
func normalizeTaskStatusTrigger(ctx context.Context, in *WorkflowInput) error {
	if strings.TrimSpace(in.TriggerType) != workflowModel.TriggerTaskStatusChanged {
		return nil
	}
	projectID, _ := in.TriggerConfig["project_id"].(string)
	toStatus, _ := in.TriggerConfig["to_status"].(string)
	f, err := taskStatusBusiness.NormalizeMoveFilter(ctx, projectID, toStatus)
	if err != nil {
		return err
	}
	for _, a := range in.Actions {
		if a.Type == ActionReply && strings.TrimSpace(in.ChannelID) == "" {
			return fmt.Errorf("choose the channel its reply posts in")
		}
	}
	cfg := map[string]interface{}{}
	if f.ProjectID != "" {
		cfg["project_id"] = f.ProjectID
	}
	if f.ToStatus != "" {
		cfg["to_status"] = f.ToStatus
	}
	in.TriggerConfig = cfg
	return nil
}

// handleTaskStatusChanged runs the task_status_changed workflows a move
// matches. Changes a workflow itself made never arrive here: its actions run
// in a context that suppresses the event bus.
func handleTaskStatusChanged(ctx context.Context, data map[string]interface{}) {
	str := func(k string) string { s, _ := data[k].(string); return s }
	move := taskStatusBusiness.MoveFromEvent(data)
	taskID := str("task_id")
	vars := map[string]string{
		"task":    str("task_name"),
		"status":  str("new_status_name"),
		"from":    str("old_status_name"),
		"project": str("project_name"),
		"by":      str("updated_by_name"),
		"link":    authService.FrontendBaseURL() + "/app/task/" + taskID,
	}
	for _, cw := range candidatesFor(workflowModel.TriggerTaskStatusChanged) {
		if !cw.taskMove.Matches(move) {
			continue
		}
		runWorkflow(ctx, cw, triggerEvent{
			channelID:      cw.channelID,
			text:           vars["task"],
			targetUserUUID: str("updated_by_uuid"),
			vars:           vars,
		})
	}
}
