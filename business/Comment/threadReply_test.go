package business

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// People in a channel thread, as uuids so they are asked about.
const (
	pMaya  = "00000000-0000-4000-8000-0000000000a1"
	pJonas = "00000000-0000-4000-8000-0000000000a2"
	pSam   = "00000000-0000-4000-8000-0000000000a3"
	pAda   = "00000000-0000-4000-8000-0000000000a4"
)

type channelTold struct {
	notices  []ReplyNotice
	emailed  []string
	items    []struct{ to, kind string }
	recorded []threadIndex.Reply
}

// replyInChannel runs a reply in a channel thread: Maya's post in #launch,
// with the thread's earlier repliers, and the router answering told. It says
// what the router was asked, who was told what and what the index was told.
func replyInChannel(t *testing.T, told ReplyTold, repliers []string, by string, mentioned ...string) channelTold {
	t.Helper()
	var mu sync.Mutex
	var got channelTold
	old := []any{threadOfPost, recordReply, tellReply, emailPostComment, publishToUser, signedProfileURL}
	t.Cleanup(func() {
		threadOfPost = old[0].(func(context.Context, string) (*dgraphStruct.DgraphPost, error))
		recordReply = old[1].(func(context.Context, threadIndex.Reply))
		tellReply = old[2].(func(context.Context, ReplyNotice) ReplyTold)
		emailPostComment = old[3].(func(string, string, string, string, string, string, string, []string))
		publishToUser = old[4].(func(string, *dgraphModels.UnifiedActivityItem))
		signedProfileURL = old[5].(func(context.Context, *string) string)
	})
	rootAt := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	post := &dgraphStruct.DgraphPost{
		Uuid:      "post-1",
		PostBy:    &dgraphStruct.DgraphUser{Uuid: pMaya},
		Channel:   &dgraphStruct.DgraphChannel{Uuid: "launch"},
		CreatedAt: &rootAt,
	}
	threadOfPost = func(_ context.Context, id string) (*dgraphStruct.DgraphPost, error) {
		thread := &dgraphStruct.DgraphPost{Uuid: id}
		for _, r := range repliers {
			thread.Comments = append(thread.Comments, &dgraphStruct.DgraphComment{CommentBy: &dgraphStruct.DgraphUser{Uuid: r}})
		}
		return thread, nil
	}
	recordReply = func(_ context.Context, r threadIndex.Reply) {
		mu.Lock()
		defer mu.Unlock()
		got.recorded = append(got.recorded, r)
	}
	tellReply = func(_ context.Context, n ReplyNotice) ReplyTold {
		mu.Lock()
		defer mu.Unlock()
		got.notices = append(got.notices, n)
		return told
	}
	emailPostComment = func(_, _, _, _, _, _, _ string, to []string) {
		mu.Lock()
		defer mu.Unlock()
		got.emailed = append(got.emailed, to...)
	}
	publishToUser = func(to string, it *dgraphModels.UnifiedActivityItem) {
		mu.Lock()
		defer mu.Unlock()
		got.items = append(got.items, struct{ to, kind string }{to, it.ActivityType})
	}
	signedProfileURL = func(context.Context, *string) string { return "" }
	var ms []*dgraphStruct.DgraphUser
	for _, m := range mentioned {
		ms = append(ms, &dgraphStruct.DgraphUser{Uuid: m})
	}
	NotifyThreadReply(ThreadReply{
		By: &dgraphStruct.DgraphUser{Uuid: by, UserName: "w"}, Post: post, CommentUUID: "c1",
		HTML: "<p>on it</p>", PlainText: "on it", Mentioned: ms, At: rootAt.Add(time.Hour),
	})
	return got
}

// A reply in a channel thread is recorded in the thread's index (chat P2):
// the channel, the post and its author, the writer and those mentioned.
func TestAChannelThreadReplyIsRecordedInTheIndex(t *testing.T) {
	got := replyInChannel(t, ReplyTold{}, nil, pJonas, pSam)
	if len(got.recorded) != 1 {
		t.Fatalf("recorded %d replies, want 1", len(got.recorded))
	}
	r := got.recorded[0]
	want := threadIndex.Thread{Kind: threadIndex.KindChannel, ID: "launch", Root: "post-1", RootBy: pMaya,
		RootAt: time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)}
	if r.Thread != want || r.By != pJonas || !slices.Equal(r.Mentioned, []string{pSam}) || !r.At.Equal(want.RootAt.Add(time.Hour)) {
		t.Errorf("recorded %+v, want thread %+v by jonas mentioning sam an hour on", r, want)
	}
}

// A reply in a channel thread goes through the one door (business/Notify,
// A4 H2): the router is asked once, with the reply as its event (named by
// post and reply, retracted by the reply's id, the post's author, those who
// replied before and those mentioned), and the email and the live Activity
// items go only to whom it told: the email to the thread's people, the reply
// item to the post's author, a mention item to each one mentioned. Who
// follows the thread or turned it off is the router's (threadAudience_test).
func TestAChannelThreadReplyGoesThroughOneDoor(t *testing.T) {
	told := ReplyTold{Thread: []string{pMaya, pSam, pAda}, RepliedTo: []string{pMaya}, Mentioned: []string{pSam}}
	got := replyInChannel(t, told, []string{pAda, pJonas}, pJonas, pSam)
	if len(got.notices) != 1 {
		t.Fatalf("asked the router %d times, want once", len(got.notices))
	}
	n := got.notices[0]
	if n.ID != "post_comment:post-1:c1" || n.MessageID != "c1" || n.Actor != pJonas || n.Author != pMaya ||
		n.ChannelID != "launch" || n.PostID != "post-1" || !slices.Equal(n.Repliers, []string{pAda, pJonas}) ||
		!slices.Equal(n.Mentions, []string{pSam}) || n.Push[firebaseInit.FIREBASE_PUSH_DATA_BODY] != "on it" {
		t.Errorf("asked the router of %+v", n)
	}
	if !slices.Equal(got.emailed, told.Thread) {
		t.Errorf("emailed %v, want whom the thread's rule told %v", got.emailed, told.Thread)
	}
	want := []struct{ to, kind string }{{pMaya, mqttStruct.MESSAGE_ACTIVITY_COMMENT}, {pSam, mqttStruct.MESSAGE_ACTIVITY_MENTION}}
	if !slices.Equal(got.items, want) {
		t.Errorf("Activity items %v, want %v", got.items, want)
	}
	// Told nobody (a double fire, or all of them off): no email, no item.
	if got := replyInChannel(t, ReplyTold{}, nil, pJonas, pSam); len(got.emailed) != 0 || len(got.items) != 0 {
		t.Errorf("told nobody, yet emailed %v and sent items %v", got.emailed, got.items)
	}
}
