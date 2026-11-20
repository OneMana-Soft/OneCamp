package business

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	domain "github.com/akashc777/OneCamp/domain/UserProjectNotification"
	"github.com/akashc777/OneCamp/helpers"
)

func CreateProjectNotificationType(userId string, projectId string, notificationType string) {
	ctx := context.Background()
	currentTime := time.Now()
	err := domain.CreateProjectNotificationType(ctx, userId, projectId, notificationType, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateProjectNotificationType Failed to create project notification err: %+v",
			err)
		return
	}
}

func UpdateProjectNotificationType(userId string, projectId string, notificationType string) (err error) {
	ctx := context.Background()

	currentTime := time.Now()
	err = domain.UpdateProjectNotificationType(ctx, userId, projectId, notificationType, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateProjectNotificationType Failed to update project notification err: %+v",
			err)
		return
	}
	return
}

func DeleteNotificationTypeWhenUserIsRemovedFormProject(userId string, projectId string) {
	ctx := context.Background()

	err := domain.DeleteNotificationTypeWhenUserIsRemovedFormProject(ctx, userId, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteNotificationTypeWhenUserIsRemovedFormProject Failed to delete user's project notification err: %+v",
			err)
		return
	}

}

func GetNotificationTypeByUserIdAndProjectId(userId string, projectId string) (notificationType string, err error) {
	ctx := context.Background()

	notificationType, err = domain.GetNotificationTypeByUserIdAndProjectId(ctx, userId, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNotificationTypeByUserIdAndProjectId Failed to delete user's project notification err: %+v",
			err)
		return
	}
	return
}

// GetEligibleUsersForProjectActivity is who of a project's people hears, by
// their setting for it, of something in it that names mentionsList, never
// excludeUserID: a mention reaches them as Named, anything else in the project
// as Plain; with limitToMentions, of the mentioned alone. Without it, the
// candidates are everyone with a setting plus everyone it names, so a named
// person with no setting (a task's assignee outside the project, or a member
// whose row was never written) hears it as their email does. One read of the
// settings, and the rule is notifylevel.Hearing (no setting hears); who may
// read it is the caller's to ask.
func GetEligibleUsersForProjectActivity(ctx context.Context, projectId string, excludeUserID string, mentionsList []string, limitToMentions bool) ([]string, error) {
	named := lowerSet(mentionsList)
	var ask []string // nil: everyone with a setting for the project
	if limitToMentions {
		if ask = lowerUnique(mentionsList); len(ask) == 0 {
			return nil, nil
		}
	}
	levels, err := projectLevels(ctx, projectId, ask)
	if err != nil {
		return nil, err
	}
	if !limitToMentions {
		ask = lowerUnique(append(slices.Sorted(maps.Keys(levels)), mentionsList...))
	}
	return hearingIn(levels, excludeUserID, ask, func(id string) notifylevel.Heard {
		if named[id] {
			return notifylevel.Named
		}
		return notifylevel.Plain
	}), nil
}

// TaskCommentAudience is who of a project's people, other than a comment's
// writer, hears of a comment on one of its tasks by their setting for the
// project: those in the task's thread hear it as Thread, those it is for
// (whom it mentions, whom the task is given to) as Named. One read of the
// settings, and the rule is notifylevel.Hearing (no setting hears). Neither
// a setting nor its lack is proof someone is in the project: whoever tells
// them asks who may read the task too.
func TaskCommentAudience(ctx context.Context, projectID string, writerID string, thread []string, forThem []string) ([]string, error) {
	named := lowerSet(forThem)
	ask := lowerUnique(append(slices.Clone(thread), forThem...))
	if len(ask) == 0 {
		return nil, nil
	}
	levels, err := projectLevels(ctx, projectID, ask)
	if err != nil {
		return nil, err
	}
	return hearingIn(levels, writerID, ask, func(id string) notifylevel.Heard {
		if named[id] {
			return notifylevel.Named
		}
		return notifylevel.Thread
	}), nil
}

// projectLevels reads people's settings for a project in one read (the
// domain logs a failure); a seam for tests. Each caller reads it itself, so
// the store-loop guard (helpers) sees the read wherever a loop makes it.
var projectLevels = domain.LevelsInProject

// hearingIn is which of ask (nil: everyone with a setting for the project,
// as levels holds them), never exclude, hear (notifylevel.Hearing) what
// reaches each as heard says. Pure.
func hearingIn(levels map[string]string, exclude string, ask []string, heard func(string) notifylevel.Heard) []string {
	ids := ask
	if ids == nil {
		ids = slices.Sorted(maps.Keys(levels))
	}
	exclude = strings.ToLower(exclude)
	ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == exclude })
	return notifylevel.Hearing(levels, ids, heard)
}

// lowerUnique is ids lower-cased (as the settings are read back), each once,
// in their order.
func lowerUnique(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.ToLower(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func lowerSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[strings.ToLower(id)] = true
	}
	return set
}
