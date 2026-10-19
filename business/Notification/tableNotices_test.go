package notification

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
)

// A notice about a table's row is pushed with the table in type_id and the
// row in thread_id, so a tap opens the row; a doc's push carries no thread.
func TestARowsNoticeIsPushedToOpenTheRow(t *testing.T) {
	old := noticeDeliver
	t.Cleanup(func() { noticeDeliver = old })
	var sent map[string]string
	noticeDeliver = func(_ context.Context, _ []string, data map[string]string, _ string) { sent = data }

	row := mention()
	row.Push = &Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_TABLE_ROW, ID: "table-1", Thread: "row-1"}
	pushNotice(context.Background(), row, []string{sam}, noticeDeliver)
	if sent[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != "table_row" || sent[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != "table-1" ||
		sent[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] != "row-1" {
		t.Errorf("a row's push: %v", sent)
	}
	pushNotice(context.Background(), mention(), []string{sam}, noticeDeliver)
	if _, ok := sent[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID]; ok {
		t.Errorf("a doc's push names a thread: %v", sent)
	}
}

// A mention on a row is emailed by the mentions setting, and a reply on one by
// the comments setting, as a doc's are.
func TestRowEmailsFollowTheirSettings(t *testing.T) {
	mentionsOnly := &prefModels.UserNotificationPreference{EmailMentions: true}
	commentsOnly := &prefModels.UserNotificationPreference{EmailComments: true}
	if !eventEnabledForPref(EventTableMention, mentionsOnly) || eventEnabledForPref(EventTableMention, commentsOnly) {
		t.Error("a mention on a row doesn't follow the mentions setting")
	}
	if !eventEnabledForPref(EventTableComment, commentsOnly) || eventEnabledForPref(EventTableComment, mentionsOnly) {
		t.Error("a reply on a row doesn't follow the comments setting")
	}
}
