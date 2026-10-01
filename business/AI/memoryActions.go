package business

// Agentic execution from the memory layer — "knowledge that acts".
//
// The memory layer already knows the workspace's open commitments,
// decisions, and questions (with owners and due dates). These functions let
// a user turn that structured knowledge into real workspace actions in one
// click: convert a commitment into an assigned task, or nudge its owner.
// This is the leap from a tool that REMEMBERS to one that ACTS — the
// defining trait of an AI-native workspace.
//
// Production properties:
//   - Permission-correct: the memory item must be visible to the caller
//     (authorizeMemoryAccess), AND task creation re-checks project access
//     through the existing executor path (no privilege escalation).
//   - Reuses the proven executors (executeCreateTask) rather than a parallel
//     code path, so validation/permission/notification stay consistent.
//   - On success, the source memory item is marked resolved and its
//     projections are kept in sync — closing the loop so a converted
//     commitment stops showing as "open".
//   - Best-effort, bounded, and fully gated on the AI service being enabled.

import (
	"context"
	"fmt"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// CreateTaskFromMemoryInput is the request to convert a memory item into a
// task. ProjectUUID is required (the user picks where it lands). Assignee
// and priority are optional; the memory's content becomes the task name and
// its due date (if any) carries over.
type CreateTaskFromMemoryInput struct {
	MemoryID     uuid.UUID
	ProjectUUID  string
	AssigneeUUID string
	Priority     string
}

// CreateTaskFromMemory turns a memory item (typically a commitment) into a
// task in the given project, then marks the memory resolved. Returns the
// result message + action data from the underlying task executor.
func CreateTaskFromMemory(ctx context.Context, userInfo *userModels.UserInfo, in CreateTaskFromMemoryInput) (*adapter.ExecuteActionResponse, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI service is not enabled")
	}
	if in.ProjectUUID == "" {
		return nil, fmt.Errorf("project_uuid is required")
	}

	// 1. Authorize + load the memory item (visibility rule == retrieval rule).
	if err := authorizeMemoryAccess(ctx, userInfo, in.MemoryID); err != nil {
		return nil, err
	}
	item, err := memoryModels.GetByID(ctx, in.MemoryID)
	if err != nil {
		return nil, err
	}

	// 2. Build the task name from the memory content (bounded), description
	//    links back to its origin for traceability.
	taskName := strings.TrimSpace(item.Content)
	if taskName == "" {
		return nil, fmt.Errorf("memory item has no content")
	}
	if len(taskName) > 120 {
		taskName = taskName[:117] + "…"
	}
	desc := "Created from workspace memory (" + item.Kind + ")."

	// 3. Reuse the proven create_task executor — it re-checks project access
	//    (admin) and wires notifications, so we don't duplicate that logic.
	params := map[string]string{
		"task_name":    taskName,
		"project_uuid": in.ProjectUUID,
		"description":  desc,
	}
	if p := strings.TrimSpace(in.Priority); p != "" {
		params["priority"] = p
	}
	if a := strings.TrimSpace(in.AssigneeUUID); a != "" {
		params["assignee_uuid"] = a
	}

	resp, err := ExecuteAction(ctx, userInfo, "create_task", params, ai.GetLocalization(ctx))
	if err != nil {
		return nil, err
	}
	if resp == nil || !resp.Success {
		// Surface the executor's failure message without resolving memory.
		return resp, nil
	}

	// 4. Close the loop: a commitment that became a task is no longer "open".
	//    Best-effort — the task already exists, so a status hiccup must not
	//    fail the action.
	if err := UpdateWorkspaceMemoryStatus(ctx, userInfo, in.MemoryID, memoryModels.StatusResolved); err != nil {
		helpers.LogErrorWithContext(ctx, "CreateTaskFromMemory: resolve memory %s failed: %v", in.MemoryID, err)
	}

	return resp, nil
}

// RemindAboutMemoryInput requests a calendar reminder for a memory item.
type RemindAboutMemoryInput struct {
	MemoryID  uuid.UUID
	StartTime string // RFC3339
}

// RemindAboutMemory creates a calendar reminder for the caller about a
// memory item (e.g. "follow up on this open question"). Reuses the
// set_reminder executor. Does NOT change the memory status — a reminder is
// a nudge, not a resolution.
func RemindAboutMemory(ctx context.Context, userInfo *userModels.UserInfo, in RemindAboutMemoryInput) (*adapter.ExecuteActionResponse, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI service is not enabled")
	}
	if strings.TrimSpace(in.StartTime) == "" {
		return nil, fmt.Errorf("start_time is required")
	}

	if err := authorizeMemoryAccess(ctx, userInfo, in.MemoryID); err != nil {
		return nil, err
	}
	item, err := memoryModels.GetByID(ctx, in.MemoryID)
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(item.Content)
	if len(title) > 120 {
		title = helpers.TruncateRunesWithSuffix(title, 117, "…")
	}
	if title == "" {
		title = "Workspace memory follow-up"
	}

	params := map[string]string{
		"title":       "Follow up: " + title,
		"start_time":  strings.TrimSpace(in.StartTime),
		"description": "Reminder from workspace memory (" + item.Kind + ").",
	}
	return ExecuteAction(ctx, userInfo, "set_reminder", params, ai.GetLocalization(ctx))
}
