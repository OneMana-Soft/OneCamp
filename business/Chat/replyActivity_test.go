package business

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// Ids as the people in the tests, uuids so they are asked about.
const (
	maya  = "00000000-0000-4000-8000-00000000000a"
	jonas = "00000000-0000-4000-8000-00000000000b"
	sam   = "00000000-0000-4000-8000-00000000000c"
	ada   = "00000000-0000-4000-8000-00000000000d"
	eve   = "00000000-0000-4000-8000-00000000000e"
	zed   = "00000000-0000-4000-8000-00000000000f"
)

type told struct {
	pushed, emailed []string
	items           []struct{ to, kind string }
}

// replyIn runs a reply by writer in the thread of message, with the
// conversation's settings levels and the thread's earlier repliers, and says
// who was told what.
func replyIn(t *testing.T, levels map[string]string, repliers []string, message *dgraphStruct.DgraphChat, writer string, mentioned ...string) told {
	t.Helper()
	var mu sync.Mutex
	var got told
	oldLevels, oldRepliers, oldDeliver, oldPublish, oldEmail := threadLevels, threadRepliers, deliverTo, publishActivityTo, emailChatComment
	t.Cleanup(func() {
		threadLevels, threadRepliers, deliverTo, publishActivityTo, emailChatComment = oldLevels, oldRepliers, oldDeliver, oldPublish, oldEmail
	})
	threadLevels = func(_ context.Context, _ string, ids []string) (map[string]string, error) {
		out := map[string]string{}
		for _, id := range ids {
			if l, ok := levels[id]; ok {
				out[strings.ToLower(id)] = l
			}
		}
		return out, nil
	}
	threadRepliers = func(context.Context, string) ([]string, error) { return repliers, nil }
	deliverTo = func(_ context.Context, to []string, _ map[string]string, _ string) {
		mu.Lock()
		defer mu.Unlock()
		got.pushed = append(got.pushed, to...)
	}
	publishActivityTo = func(to string, it *dgraphModels.UnifiedActivityItem) {
		mu.Lock()
		defer mu.Unlock()
		got.items = append(got.items, struct{ to, kind string }{to, it.ActivityType})
	}
	emailChatComment = func(_, _, _, _, _, _, _ string, to []string) {
		mu.Lock()
		defer mu.Unlock()
		got.emailed = append(got.emailed, to...)
	}
	var ms []*dgraphStruct.DgraphUser
	for _, m := range mentioned {
		ms = append(ms, &dgraphStruct.DgraphUser{Uuid: m})
	}
	notifyChatThreadReply(time.Now(), "c1", "<p>on it</p>", &dgraphStruct.DgraphUser{Uuid: writer, UserName: "w"}, "grp", ms, message)
	return got
}

func group(from string, people ...string) *dgraphStruct.DgraphChat {
	dm := &dgraphStruct.DgraphDm{}
	for _, p := range people {
		dm.Participants = append(dm.Participants, &dgraphStruct.DgraphUser{Uuid: p})
	}
	return &dgraphStruct.DgraphChat{Uuid: "m1", From: &dgraphStruct.DgraphUser{Uuid: from}, DM: dm}
}

// A reply in a group thread reaches one audience by every way: push and
// desktop notice, Activity and email (BE-11-G-1). Push used to go to everyone
// in the group on "all", Activity and email to the author and those
// mentioned.
func TestAGroupThreadReplyReachesOneAudience(t *testing.T) {
	levels := map[string]string{maya: "all", jonas: "all", sam: "mention", ada: "all", eve: "block", zed: "all"}
	got := replyIn(t, levels, []string{ada, eve, jonas}, group(maya, maya, jonas, sam, ada, eve, zed), jonas, sam)
	want := []string{maya, ada, sam}
	if !slices.Equal(got.pushed, want) {
		t.Errorf("pushed %v, want %v: maya wrote the message, ada replied, sam was mentioned; never jonas (wrote it), eve (Nothing, not addressed) or zed (not in the thread)", got.pushed, want)
	}
	if !slices.Equal(got.emailed, got.pushed) {
		t.Errorf("emailed %v, pushed %v: one audience", got.emailed, got.pushed)
	}
	wantItems := []struct{ to, kind string }{{sam, mqttStruct.MESSAGE_ACTIVITY_MENTION}, {maya, mqttStruct.MESSAGE_ACTIVITY_COMMENT}}
	if !slices.Equal(got.items, wantItems) {
		t.Errorf("Activity items %v, want %v", got.items, wantItems)
	}
}

// Someone mentioned who isn't in the group hears nothing of it.
func TestAGroupThreadReplyNeverReachesOutsiders(t *testing.T) {
	levels := map[string]string{maya: "all", jonas: "all"}
	got := replyIn(t, levels, nil, group(maya, maya, jonas, ada), jonas, zed)
	if !slices.Equal(got.pushed, []string{maya}) || slices.Contains(got.emailed, zed) {
		t.Errorf("pushed %v, emailed %v: zed is not in the group", got.pushed, got.emailed)
	}
}

// In a DM the other person is in every thread of it: Maya's reply under her
// own message reaches Jonas, unless he chose to hear nothing of the DM.
func TestADMThreadReplyReachesTheOtherPerson(t *testing.T) {
	dm := group(maya, maya, jonas)
	if got := replyIn(t, map[string]string{maya: "all", jonas: "all"}, nil, dm, maya); !slices.Equal(got.pushed, []string{jonas}) || len(got.items) != 0 {
		t.Errorf("pushed %v, items %v: want jonas pushed, and no Activity item (not the author, not mentioned)", got.pushed, got.items)
	}
	if got := replyIn(t, map[string]string{maya: "all", jonas: "block"}, nil, dm, maya); len(got.pushed) != 0 {
		t.Errorf("jonas, on Nothing, was pushed: %v", got.pushed)
	}
	// Jonas replies to Maya's message: she gets the reply item, he nothing.
	got := replyIn(t, map[string]string{maya: "all", jonas: "all"}, nil, dm, jonas)
	if !slices.Equal(got.pushed, []string{maya}) || len(got.items) != 1 || got.items[0].to != maya || got.items[0].kind != mqttStruct.MESSAGE_ACTIVITY_COMMENT {
		t.Errorf("pushed %v, items %v", got.pushed, got.items)
	}
}

// When who hears of it can't be read, nobody is told.
func TestAThreadReplyWhoseAudienceFailsTellsNobody(t *testing.T) {
	_ = replyIn(t, nil, nil, group(maya, maya, jonas), jonas)
	threadLevels = func(context.Context, string, []string) (map[string]string, error) { return nil, errors.New("down") }
	var pushed bool
	deliverTo = func(context.Context, []string, map[string]string, string) { pushed = true }
	notifyChatThreadReply(time.Now(), "c1", "<p>x</p>", &dgraphStruct.DgraphUser{Uuid: jonas}, "grp", nil, group(maya, maya, jonas))
	if pushed {
		t.Error("told someone with no audience")
	}
}
