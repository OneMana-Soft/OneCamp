package business

import (
	"context"
	"fmt"
	"slices"
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

// handleTaskEvent runs the rules a task event triggers: a project's rules
// for its tasks, and workspace workflows scoped by their trigger config. The
// task itself is read as each rule's owner when the rule runs (runs.go), so
// what an event says is only what woke the rule.
func handleTaskEvent(ctx context.Context, trigger string, data map[string]interface{}) {
	str := func(k string) string { s, _ := data[k].(string); return s }
	taskID := str("task_id")
	if taskID == "" {
		return
	}
	by := str("updated_by_uuid")
	for _, k := range []string{"assigned_by", "created_by"} {
		if by == "" {
			by = str(k)
		}
	}
	vars := taskEventVars(data)
	name := vars["task"]
	move := taskStatusBusiness.MoveFromEvent(data)
	projectID := str("project_id")
	key := eventKey(trigger, data)
	for _, cw := range candidatesFor(trigger) {
		if cw.projectID != "" && cw.projectID != projectID {
			continue
		}
		if !cw.wants(trigger, data, move) {
			continue
		}
		runWorkflow(ctx, cw, triggerEvent{
			channelID:      cw.channelID,
			projectID:      projectID,
			taskID:         taskID,
			text:           name,
			targetUserUUID: by,
			vars:           vars,
			eventKey:       key,
		})
	}
}

// wants says whether a rule cares about this task event beyond its kind:
// the move it names, the fields it watches.
// taskEventVars is what a rule's words may say of a task event: {task},
// {status}, {from}, {project}, {by} and {link}, and for a form's, {form} and
// each answer as {answer:<question id>}.
func taskEventVars(data map[string]interface{}) map[string]string {
	str := func(k string) string { s, _ := data[k].(string); return s }
	taskID := str("task_id")
	vars := map[string]string{
		"task":    firstOf(str("task_name"), str("name")),
		"status":  str("new_status_name"),
		"from":    str("old_status_name"),
		"project": str("project_name"),
		"by":      firstOf(str("updated_by_name"), str("assigned_by_name")),
		"link":    authService.FrontendBaseURL() + "/app/task/" + taskID,
	}
	if form := str("form_title"); form != "" {
		vars["form"] = form
	}
	// An answered approval: who answered last, how it ended, and what they said.
	if outcome := str("outcome"); outcome != "" {
		vars["approver"] = str("approver")
		vars["outcome"] = map[string]string{"approved": "approved", "changes": "asked for changes", "rejected": "rejected"}[outcome]
		vars["comment"] = str("comment")
	}
	switch answers := data["answers"].(type) {
	case map[string]interface{}:
		for id, v := range answers {
			if s, ok := v.(string); ok {
				vars["answer:"+id] = s
			}
		}
	case map[string]string:
		for id, s := range answers {
			vars["answer:"+id] = s
		}
	}
	return vars
}

func (cw *compiledWorkflow) wants(trigger string, data map[string]interface{}, move taskStatusBusiness.Move) bool {
	switch trigger {
	case workflowModel.TriggerFormSubmitted:
		f, _ := data["form_id"].(string)
		return cw.formID != "" && f == cw.formID
	case workflowModel.TriggerApprovalDecided:
		o, _ := data["outcome"].(string)
		return cw.outcome == "" || o == cw.outcome
	case workflowModel.TriggerTaskStatusChanged:
		return cw.taskMove.Matches(move)
	case workflowModel.TriggerTaskFieldChanged:
		if len(cw.fields) == 0 {
			return true
		}
		f, _ := data["field"].(string)
		return slices.Contains(cw.fields, f)
	}
	return true
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
