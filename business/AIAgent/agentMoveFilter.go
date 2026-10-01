package business

import (
	"context"
	"strings"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// eventTaskStatusChanged is the event an agent can narrow to one project and
// one status ("when a task moves to QA"), the same way a workflow does.
// Unnarrowed, an agent on it ran a model call for every status change in the
// workspace.
const eventTaskStatusChanged = "task.status_changed"

// normalizeAgentMoveFilter checks and rewrites an event agent's project and
// status before it is saved (see TaskStatus.NormalizeMoveFilter). Other
// triggers and events keep their config untouched, apart from dropping a stale
// project or status left from a previous choice.
func normalizeAgentMoveFilter(ctx context.Context, in *AgentInput) error {
	if in.TriggerConfig == nil {
		return nil
	}
	event, _ := in.TriggerConfig["event"].(string)
	if strings.TrimSpace(in.TriggerType) != model.TriggerEvent || strings.TrimSpace(event) != eventTaskStatusChanged {
		delete(in.TriggerConfig, "project_id")
		delete(in.TriggerConfig, "to_status")
		return nil
	}
	projectID, _ := in.TriggerConfig["project_id"].(string)
	toStatus, _ := in.TriggerConfig["to_status"].(string)
	f, err := taskStatusBusiness.NormalizeMoveFilter(ctx, projectID, toStatus)
	if err != nil {
		return err
	}
	delete(in.TriggerConfig, "project_id")
	delete(in.TriggerConfig, "to_status")
	if f.ProjectID != "" {
		in.TriggerConfig["project_id"] = f.ProjectID
	}
	if f.ToStatus != "" {
		in.TriggerConfig["to_status"] = f.ToStatus
	}
	return nil
}

// eventWanted reports whether an event agent wants this occurrence of its
// event. Only a status change can be narrowed today; every other event is
// wanted as before.
func eventWanted(a *model.AiAgent, eventType string, data map[string]interface{}) bool {
	if eventType != eventTaskStatusChanged {
		return true
	}
	return parseTriggerConfig(a).MoveFilter.Matches(taskStatusBusiness.MoveFromEvent(data))
}
