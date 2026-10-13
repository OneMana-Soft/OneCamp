package business

import (
	"context"
	"slices"
	"testing"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A message forwarded into DMs and groups is told of through Deliver, one
// call per conversation, with the notice id a new message there has, so the
// desktop app gets its notice too (BE-11-G-2).
func TestAForwardIsDeliveredLikeANewMessage(t *testing.T) {
	type call struct {
		to       []string
		threadID string
		typeID   string
		noticeID string
	}
	var calls []call
	oldEligible, oldDeliver := eligibleForForward, deliverForward
	t.Cleanup(func() { eligibleForForward, deliverForward = oldEligible, oldDeliver })
	eligibleForForward = func(_ context.Context, grpId, sender string, _ []string, _ bool) ([]string, error) {
		if sender != "maya" {
			t.Errorf("the sender was %q", sender)
		}
		return map[string][]string{"maya jonas": {"jonas"}, "g1": {"ada", "bo"}}[grpId], nil
	}
	deliverForward = func(_ context.Context, to []string, data map[string]string, noticeID string) {
		calls = append(calls, call{to, data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID], data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID], noticeID})
	}
	sendFwdMessageNotifications(context.Background(), &dgraphStruct.DgraphUser{Uuid: "maya", UserName: "maya"}, "<p>fyi</p>",
		[]*dgraphStruct.DgraphDm{dmNode("maya jonas", "c1"), dmNode("g1", "c2"), dmNode("g-muted", "c3"), {GroupingId: "empty"}})
	want := []call{
		{[]string{"jonas"}, "c1", "maya jonas", "chat:maya jonas:c1"},
		{[]string{"ada", "bo"}, "c2", "g1", "chat:g1:c2"},
	}
	if !slices.EqualFunc(calls, want, func(a, b call) bool {
		return slices.Equal(a.to, b.to) && a.threadID == b.threadID && a.typeID == b.typeID && a.noticeID == b.noticeID
	}) {
		t.Errorf("delivered %+v, want %+v (nobody for a conversation with nobody to tell, or no message)", calls, want)
	}
}
