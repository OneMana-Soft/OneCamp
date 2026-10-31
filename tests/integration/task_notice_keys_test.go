//go:build integration
// +build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	_ "github.com/akashc777/OneCamp/business/Workflow" // the inbox kind that names a task's notices
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// A task's notice names it by its key then its name in the inbox (linear-02:
// keys everywhere), as its live item does: WEB-1 Write the launch post. A
// reader who may not open the task gets neither, if told at all.
func TestATasksNoticeIsNamedByItsKey(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	if err := taskKeys.Settle(ctx, uuid.MustParse(w.project)); err != nil {
		t.Fatal(err)
	}
	key := taskKeys.KeyOf(ctx, uuid.MustParse(w.task))
	if key == "" {
		t.Fatal("ana's task has no key")
	}
	named := key + " Write the launch post"
	if live := taskKeys.NamedTask(ctx, w.task, "Write the launch post"); live != named {
		t.Errorf("the live item is titled %q, want %q", live, named)
	}

	// A rule tells ben, a member, and cy, who isn't in the project.
	notificationBusiness.Tell(ctx, notificationBusiness.Notice{
		Kind: "work.rule", ResourceKind: "task", ResourceUUID: w.task, ResourceTitle: named,
		DedupKey: "rule|keys|" + w.task, Title: "A rule's note", Path: "/app/task/" + w.task,
	}, []notificationBusiness.NoticeRecipient{{UserUUID: w.people["ben"].String(), Reason: "rule"}, {UserUUID: w.people["cy"].String(), Reason: "rule"}})
	inbox := func(who string) *dgraphModels.NoticeItem {
		page := activityBusiness.WithNotices(ctx, dgraphModels.UnifiedActivityPagination{}, w.people[who].String(), time.Now().Add(time.Hour), 20)
		for _, a := range page.Activities {
			if a.Notice != nil && a.Notice.ResourceUUID == w.task {
				return a.Notice
			}
		}
		return nil
	}
	if n := inbox("ben"); n == nil || !n.CanOpen || n.Title != named {
		t.Errorf("ben's notice: %+v, want it titled %q", n, named)
	}
	if n := inbox("cy"); n != nil && (n.CanOpen || n.Title != "") {
		t.Errorf("cy's notice names a task he can't open: %+v", n)
	}
}
