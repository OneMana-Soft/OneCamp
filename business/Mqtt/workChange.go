package business

import (
	"context"
	"encoding/json"

	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// WorkChange is one Work Change Log entry (business/WorkFeed) for its
// project's open views: its number in the log, its kind, the task, the
// fields' new values in the app's own answer names, and who made it.
type WorkChange struct {
	Sequence    int64
	Kind        string
	TaskUUID    string
	ProjectUUID string
	Diff        json.RawMessage
	By          string
}

// PublishWorkChange tells a change's project about it, as MESSAGE_TASK_CHANGED
// carrying the entry, so open boards and lists show it without reading their
// lists again. A task only a few may see is told as Reread, naming nothing
// (publishToProject's one rule).
func PublishWorkChange(ctx context.Context, c WorkChange) {
	msg := &mqttStruct.MqttTaskChanged{TaskUuid: c.TaskUUID, ProjectUuid: c.ProjectUUID,
		Sequence: c.Sequence, Kind: c.Kind, Diff: c.Diff, By: c.By}
	publishToProject(ctx, c.ProjectUUID, mqttStruct.MESSAGE_TASK_CHANGED, msg, "PublishWorkChange", c.TaskUUID)
}

// PublishProjectReread tells a project's open views to read their tasks
// again, naming none: a burst of changes too big to tell one by one.
func PublishProjectReread(ctx context.Context, projectUUID string) {
	if projectUUID == "" {
		return
	}
	sendToProject(ctx, projectUUID, mqttStruct.MESSAGE_TASK_CHANGED,
		&mqttStruct.MqttTaskChanged{ProjectUuid: projectUUID, Reread: true}, "PublishProjectReread")
}
