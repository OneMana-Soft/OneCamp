package business

import (
	"context"
	"testing"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A recap is remembered only inside the conversation it went to. A call that no
// conversation holds (an event's organiser alone with guests by email, an event
// inviting more people than a conversation holds, a meeting) is remembered by
// no one: a memory with no channel or chat is stored unscoped, for the whole
// workspace to recall.
func TestARecapIsRememberedOnlyInTheConversationItWentTo(t *testing.T) {
	channel := uuid.NewString()
	dm := helpers.GetGroupingId(uuid.NewString(), uuid.NewString())
	group := "abcdef0123456789abcdef0123456789"
	cases := []struct {
		name          string
		conv          conversationBusiness.Ref
		channel, chat string
		ok            bool
	}{
		{"a channel's call", conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: channel}, channel, "", true},
		{"a DM's call", conversationBusiness.ChatRef(dm), "", dm, true},
		{"a group's call, or an event's people's", conversationBusiness.ChatRef(group), "", group, true},
		{"an event's call that no conversation holds", conversationBusiness.Ref{}, "", "", false},
		{"a meeting's call", conversationBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: uuid.NewString()}, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, ok := recapMemoryScope(tc.conv, "the-room")
			if ok != tc.ok || scope.ChannelUUID != tc.channel || scope.ChatGrpID != tc.chat {
				t.Fatalf("recapMemoryScope(%+v) = %+v, %v; want channel %q, chat %q, %v", tc.conv, scope, ok, tc.channel, tc.chat, tc.ok)
			}
			if ok && (scope.SourceType != "recap" || scope.SourceUUID != "recap:the-room") {
				t.Fatalf("a recap's memory says where it came from: %+v", scope)
			}
		})
	}

	// And nothing is read or remembered for a recap that no conversation holds;
	// a group's recap is remembered once, in the group, as its author's.
	var remembered []MemoryScope
	authorReads := 0
	wasRemember, wasAuthor := rememberRecap, recapAuthorOf
	t.Cleanup(func() { rememberRecap, recapAuthorOf = wasRemember, wasAuthor })
	rememberRecap = func(_ context.Context, _ string, s MemoryScope) (int, error) {
		remembered = append(remembered, s)
		return 1, nil
	}
	author := uuid.NewString()
	recapAuthorOf = func(context.Context, string) (*userModels.UserInfo, error) {
		authorReads++
		return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: author}}, nil
	}
	for _, conv := range []conversationBusiness.Ref{{}, {Kind: callStateBusiness.KindMeeting, ID: uuid.NewString()}} {
		extractMemoryFromRecap(context.Background(), conv, "meet-"+uuid.NewString(), "0x1", "We agreed to ship on Friday.")
	}
	if len(remembered) != 0 || authorReads != 0 {
		t.Fatalf("a recap no conversation holds was remembered %d times (%+v), its author read %d times; want neither", len(remembered), remembered, authorReads)
	}
	extractMemoryFromRecap(context.Background(), conversationBusiness.ChatRef(group), "a-room", "0x1", "We agreed to ship on Friday.")
	if len(remembered) != 1 || remembered[0].ChatGrpID != group || remembered[0].ChannelUUID != "" || remembered[0].CreatedByUUID != author {
		t.Fatalf("a group's recap was remembered as %+v; want once, in the group, as its author's", remembered)
	}
}
