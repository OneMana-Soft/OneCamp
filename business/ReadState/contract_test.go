package business

import (
	"context"
	"errors"
	"testing"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The counts are the sidebar's, one per conversation, a DM told from a group
// by its grouping id.
func TestTheCountsAreTheSidebars(t *testing.T) {
	user := &dgraphStruct.DgraphUser{
		Channels: []*dgraphStruct.DgraphChannel{{Uuid: "c1", UnreadPostCount: 3}, {Uuid: "c2"}, nil},
		DMs:      []*dgraphStruct.DgraphDm{{GroupingId: "a b", UnreadMessageCount: 2}, {GroupingId: "g", UnreadMessageCount: 1}, {}},
	}
	got := countsOf(user)
	want := []Count{
		{conversationBusiness.Ref{Kind: "channel", ID: "c1"}, 3},
		{conversationBusiness.Ref{Kind: "channel", ID: "c2"}, 0},
		{conversationBusiness.Ref{Kind: "dm", ID: "a b"}, 2},
		{conversationBusiness.Ref{Kind: "group", ID: "g"}, 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if countsOf(nil) != nil {
		t.Error("nobody has no counts")
	}
}

// Threads and the Inbox have no marker of their own yet, and nobody marks
// anything.
func TestWhatHasNoMarkerYet(t *testing.T) {
	who := &userModels.UserInfo{}
	for _, ref := range []conversationBusiness.Ref{{Kind: "thread", ID: "t"}, {Kind: "call", ID: "c"}, {Kind: "channel", ID: "not a uuid"}} {
		if err := States.MarkRead(context.Background(), who, ref); !errors.Is(err, conversationBusiness.ErrNotFound) {
			t.Errorf("%+v: %v", ref, err)
		}
	}
	if err := States.MarkRead(context.Background(), nil, conversationBusiness.Ref{Kind: "dm", ID: "a b"}); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("nobody: %v", err)
	}
}
