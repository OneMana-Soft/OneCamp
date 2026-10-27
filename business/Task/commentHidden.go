package business

import (
	"context"
	"time"

	"github.com/google/uuid"

	businessComment "github.com/akashc777/OneCamp/business/Comment"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// HideTaskComment hides a task's comment (business/Comment HideComment,
// marked by) and tells its project's open views to drop it, as a person's
// delete does: the message names the comment alone.
func HideTaskComment(ctx context.Context, commentUUID uuid.UUID, projectUUID string, at time.Time, by string) (bool, error) {
	hidden, err := businessComment.HideComment(ctx, commentUUID, at, by)
	if err != nil || !hidden {
		return false, err
	}
	mqttBusiness.PublishTaskComment(&mqttStruct.MqttTaskComment{Type: mqttStruct.TYPE_DELETE, CommentUuid: commentUUID.String(), UpdatedAt: &at}, projectUUID)
	return true, nil
}

// ShowTaskComment brings back a task's comment HideTaskComment hid, as the
// task is now (away: in the trash, or archived with its project, the comment
// stays out of sight with it), and tells the task's open views to read it
// again: its project's, or, while the task is kept to some people, a message
// that names no task.
func ShowTaskComment(ctx context.Context, commentUUID uuid.UUID, taskUUID, projectUUID string, at time.Time, by string, away businessComment.Away) (bool, error) {
	shown, err := businessComment.ShowComment(ctx, commentUUID, at, by, away)
	if err != nil || !shown {
		return false, err
	}
	mqttBusiness.PublishTaskChanged(ctx, &mqttStruct.MqttTaskChanged{TaskUuid: taskUUID, ProjectUuid: projectUUID})
	return true, nil
}
