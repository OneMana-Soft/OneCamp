package business

import (
	"context"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// taskMentionNotice is how a mention in a task's words reads (task.mentioned):
// in the task's description, or in its comment commentUUID ("" for the
// description). The graph's feed lists no mention in a task's description, so
// this notice is how the person it names finds it.
func taskMentionNotice(actor *dgraphStruct.DgraphUser, task *dgraphStruct.DgraphTask, commentUUID, html string) notificationBusiness.Notice {
	where := task.Name
	if where == "" {
		where = "a task"
	}
	n := notificationBusiness.Notice{
		Kind:          notificationBusiness.TaskMentioned,
		ResourceKind:  "task",
		ResourceUUID:  task.Uuid,
		ResourceTitle: task.Name,
		Actor:         actor,
		Title:         actor.DisplayName() + " mentioned you in " + where,
		Body:          helpers.HTMLToPlainText(html),
		Path:          "/app/task/" + task.Uuid,
		Email:         notificationBusiness.EventTaskMention,
		Push:          &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK, ID: task.Uuid},
	}
	if commentUUID != "" {
		n.Data = map[string]string{"comment_uuid": commentUUID}
		projectUUID := ""
		if task.Project != nil {
			projectUUID = task.Project.Uuid
		}
		n.Push = &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK_COMMENT, ID: projectUUID, Thread: task.Uuid}
	}
	return n
}

// tellTaskMentionsAdded tells, in the background, whom a save of a task's
// words newly names (s, read before the save's write) who hears of a mention in
// its project by their setting and may read the task now: the rule a new
// task's mention follows (taskHearers).
func tellTaskMentionsAdded(ctx context.Context, actor *dgraphStruct.DgraphUser, task *dgraphStruct.DgraphTask, commentUUID, html string, s notificationBusiness.MentionSave) {
	if actor == nil || task == nil || task.Project == nil || task.Project.Uuid == "" {
		return
	}
	s.Hear = func(ctx context.Context, added []string) ([]string, error) {
		return taskHearers(ctx, task.Project.Uuid, task.Uuid, actor.Uuid, added, true)
	}
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("business/Task tellTaskMentionsAdded", func() {
		notificationBusiness.TellMentionsAdded(ctx, taskMentionNotice(actor, task, commentUUID, html), s)
	})
}
