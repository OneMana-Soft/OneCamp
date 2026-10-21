package business

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
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
	pushed, emailed []string
	items           []struct{ to, kind string }
	recorded        []threadIndex.Reply
}

// replyInChannel runs a reply in a channel thread: Maya's post in #launch,
// with the thread's earlier repliers, its index's followers and the members'
// settings, and says who was told what and what the index was told.
func replyInChannel(t *testing.T, levels map[string]string, repliers, on, off []string, by string, mentioned ...string) channelTold {
	t.Helper()
	var mu sync.Mutex
	var got channelTold
	old := []any{threadOfPost, levelsInChannel, recordReply, threadFollows, deliverReply, emailPostComment, publishToUser, signedProfileURL}
	t.Cleanup(func() {
		threadOfPost = old[0].(func(context.Context, string) (*dgraphStruct.DgraphPost, error))
		levelsInChannel = old[1].(func(context.Context, string, []string) (map[string]string, error))
		recordReply = old[2].(func(context.Context, threadIndex.Reply))
		threadFollows = old[3].(func(context.Context, string) ([]string, []string, error))
		deliverReply = old[4].(func(context.Context, []string, map[string]string, string))
		emailPostComment = old[5].(func(string, string, string, string, string, string, string, []string))
		publishToUser = old[6].(func(string, *dgraphModels.UnifiedActivityItem))
		signedProfileURL = old[7].(func(context.Context, *string) string)
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
	levelsInChannel = func(_ context.Context, _ string, ids []string) (map[string]string, error) {
		out := map[string]string{}
		for _, id := range ids {
			if l, ok := levels[id]; ok {
				out[strings.ToLower(id)] = l
			}
		}
		return out, nil
	}
	threadFollows = func(_ context.Context, root string) ([]string, []string, error) {
		if root != post.Uuid {
			t.Errorf("follows read for %q, want the post %q", root, post.Uuid)
		}
		return on, off, nil
	}
	recordReply = func(_ context.Context, r threadIndex.Reply) {
		mu.Lock()
		defer mu.Unlock()
		got.recorded = append(got.recorded, r)
	}
	deliverReply = func(_ context.Context, to []string, _ map[string]string, _ string) {
		mu.Lock()
		defer mu.Unlock()
		got.pushed = append(got.pushed, to...)
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
	levels := map[string]string{pMaya: "all", pJonas: "all", pSam: "all"}
	got := replyInChannel(t, levels, nil, nil, nil, pJonas, pSam)
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

// Who follows a channel thread by hand hears of its replies; who turned it
// off doesn't, even its author, unless the reply mentions them.
func TestFollowingAChannelThreadDecidesWhoHears(t *testing.T) {
	levels := map[string]string{pMaya: "all", pJonas: "all", pSam: "all", pAda: "all"}
	got := replyInChannel(t, levels, []string{pAda}, []string{pSam}, []string{pMaya, pAda}, pJonas)
	if !slices.Equal(got.pushed, []string{pSam}) || !slices.Equal(got.emailed, got.pushed) {
		t.Errorf("pushed %v, emailed %v: want sam alone, who follows it; maya and ada turned it off", got.pushed, got.emailed)
	}
	for _, it := range got.items {
		if it.to == pMaya && it.kind == mqttStruct.MESSAGE_ACTIVITY_COMMENT {
			t.Error("maya turned her thread off, and still got the reply item")
		}
	}
	if got := replyInChannel(t, levels, []string{pAda}, nil, []string{pAda}, pJonas, pAda); !slices.Contains(got.pushed, pAda) {
		t.Errorf("pushed %v: ada turned it off, but this reply mentions her", got.pushed)
	}
}
