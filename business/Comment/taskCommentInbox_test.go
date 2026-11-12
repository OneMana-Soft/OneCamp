package business

import (
	"context"
	"slices"
	"testing"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestATaskCommentIsAddressedToItsAssigneeElseItsCreator(t *testing.T) {
	olu, maya := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "olu"}, &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "maya"}
	for _, c := range []struct {
		task *dgraphStruct.DgraphTask
		want *dgraphStruct.DgraphUser
	}{
		{&dgraphStruct.DgraphTask{Assignee: olu, CreatedBy: maya}, olu},
		{&dgraphStruct.DgraphTask{Assignee: &dgraphStruct.DgraphUser{}, CreatedBy: maya}, maya},
		{&dgraphStruct.DgraphTask{CreatedBy: maya}, maya},
		{&dgraphStruct.DgraphTask{}, nil},
		{nil, nil},
	} {
		if got := Addressee(c.task); got != c.want {
			t.Errorf("Addressee(%+v) = %+v, want %+v", c.task, got, c.want)
		}
	}
}

// Everyone pushed or emailed who may read the task gets an inbox item of the
// comment, once, but whom the feed lists it for: its addressee, the people it
// mentions and its writer.
func TestATaskCommentsInboxIsForTheRestOfWhomItTold(t *testing.T) {
	oldReaders, oldInbox := taskCommentReaders, taskCommentInbox
	t.Cleanup(func() { taskCommentReaders, taskCommentInbox = oldReaders, oldInbox })
	var asked []string
	taskCommentReaders = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		asked = append(asked, ids...)
		return map[string]bool{"ben": true, "tom": true, "olu": true, "sam": true}, nil
	}
	var told []notificationBusiness.NoticeRecipient
	var notice notificationBusiness.Notice
	taskCommentInbox = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) {
		notice, told = n, to
	}
	task := &dgraphStruct.DgraphTask{Uuid: "kit", Name: "Press kit", Assignee: &dgraphStruct.DgraphUser{Uuid: "olu"}}
	maya := &dgraphStruct.DgraphUser{Uuid: "maya", UserName: "maya"}
	tellTaskCommentInbox(context.Background(), task, maya, "c1", []*dgraphStruct.DgraphUser{{Uuid: "sam"}},
		[]string{"ben", "olu", "sam"}, []string{"ben", "tom", "lee", "olu", "sam", "maya"})
	if want := []string{"ben", "tom", "lee"}; !slices.Equal(asked, want) {
		t.Errorf("asked who may read it of %v, want %v: never the addressee, the mentioned or the writer, each once", asked, want)
	}
	var got []string
	for _, r := range told {
		if !r.InboxOnly {
			t.Errorf("%s would be pushed or emailed twice", r.UserUUID)
		}
		got = append(got, r.UserUUID)
	}
	if want := []string{"ben", "tom"}; !slices.Equal(got, want) {
		t.Errorf("inbox items for %v, want %v: Lee may not read the task", got, want)
	}
	if notice.Kind != TaskCommented || notice.ResourceKind != "task" || notice.ResourceUUID != "kit" || notice.DedupKey != "comment|c1" ||
		notice.Data["comment_uuid"] != "c1" || notice.Path != "/app/task/kit" {
		t.Errorf("notice %+v", notice)
	}
}
