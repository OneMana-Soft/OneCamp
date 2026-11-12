//go:build integration
// +build integration

package integration_test

// Told means listed (NOTIF-H2): whoever a task comment is pushed or emailed
// to finds it in their Inbox after a reload, and on its badge. The world is
// task_push_test.go's: Maya's "Press kit" is given to Olu, and Ben, Tom and
// Lee commented on it before (Lee is off the project now).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestATaskCommentIsListedForWhomItTold' -v

import (
	"context"
	"testing"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// listedFor is whether name's Inbox (the feed with its notices, as the app
// reads it) lists the comment, and their badge.
func (w *taskWorld) listedFor(t *testing.T, name, commentUUID string) (bool, uint64) {
	t.Helper()
	ctx := context.Background()
	page, err := activityBusiness.GetUnifiedActivity(ctx, activityBusiness.FeedQuery{User: w.people[name], DgraphID: w.uids[name],
		Before: time.Now().Add(time.Minute), Limit: 20, Notices: true})
	if err != nil {
		t.Fatalf("%s's feed: %v", name, err)
	}
	badge, err := activityBusiness.UnreadActivityCount(ctx, w.people[name], w.uids[name])
	if err != nil {
		t.Fatalf("%s's badge: %v", name, err)
	}
	return listsComment(page.Activities, commentUUID), badge
}

// listsComment is whether items hold the comment: as a comment, or as a
// notice of it.
func listsComment(items []dgraphModels.UnifiedActivityItem, commentUUID string) bool {
	for _, it := range items {
		if it.Comment != nil && it.Comment.Uuid == commentUUID {
			return true
		}
		if it.Notice != nil && it.Notice.Data["comment_uuid"] == commentUUID {
			return true
		}
	}
	return false
}

// Maya comments on Olu's task. Olu (given it) heard live, by the desktop app;
// Ben (who commented before, everything) was pushed and emailed; Tom (who
// commented before, mentions only) was emailed. The comment was listed for
// none of them after a reload, nor on a badge: the graph listed a task's
// comment for its writer, and nobody's feed lists their own.
func TestATaskCommentIsListedForWhomItTold(t *testing.T) {
	w := newTaskWorld(t)
	out := w.call(t, taskController.CreateTaskComment, map[string]any{"task_uuid": w.kit, "task_comment_body": "<p>Draft is in the drive</p>"})
	id := out["comment_id"]
	if !within(20*time.Second, func() bool { return len(w.pushedTo("task_comment", w.kit)) > 0 }) {
		t.Fatal("the comment was pushed to nobody")
	}
	for _, name := range []string{"olu", "ben", "tom"} {
		if listed, badge := w.listedFor(t, name, id); !listed || badge != 1 {
			t.Errorf("%s was told of the comment; listed %v, badge %d; want it listed and a badge of 1", name, listed, badge)
		}
	}
	for _, name := range []string{"maya", "kay", "lee"} {
		if listed, badge := w.listedFor(t, name, id); listed || badge != 0 {
			t.Errorf("%s was not told of the comment; listed %v, badge %d; want neither", name, listed, badge)
		}
	}
}
