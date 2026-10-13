package business

import (
	"context"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A poll in a DM or a group chat (MC-BUG-3) is posted to the other person, or
// to the group, and stored with no channel.
func TestAConversationsPollGoesWhereItWasAsked(t *testing.T) {
	maya, jonas := "a11ce000-0000-4000-8000-00000000000a", "b0b00000-0000-4000-8000-00000000000b"
	if to, dm := dmOther(maya+" "+jonas, maya); !dm || to != jonas {
		t.Errorf("Maya's DM with Jonas posts to %q (dm %v)", to, dm)
	}
	if to, dm := dmOther(maya+" "+jonas, jonas); !dm || to != maya {
		t.Errorf("Jonas's DM with Maya posts to %q (dm %v)", to, dm)
	}
	if to, dm := dmOther(maya+" "+maya, maya); !dm || to != maya {
		t.Errorf("a DM with oneself posts to %q (dm %v)", to, dm)
	}
	if _, dm := dmOther("4f1c9e0a2b", maya); dm {
		t.Error("a group chat's id was read as a DM")
	}
	me := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: maya}}
	if !takesPartIn(context.Background(), maya+" "+jonas, me) || takesPartIn(context.Background(), jonas+" "+"c0de0000-0000-4000-8000-00000000000c", me) {
		t.Error("a DM is posted in by its two people, and by no one else")
	}
	if !inConversation(&pollModel.Poll{ChannelID: uuid.Nil}) || inConversation(&pollModel.Poll{ChannelID: uuid.New()}) {
		t.Error("a poll with no channel is a conversation's, and only that")
	}
}

// Open views hear of a vote on the poll's own place, and only its author can
// close a conversation's poll: a DM has no admins.
func TestAConversationsPollIsClosedByItsAuthorAndHeardWhereItIs(t *testing.T) {
	author, other := uuid.New(), uuid.New()
	ch := uuid.New()
	channelPoll := &pollModel.Poll{ChannelID: ch, CreatedBy: author}
	dmPoll := &pollModel.Poll{ChannelID: uuid.Nil, CreatedBy: author}
	inChannel := where{channel: &dgraphStruct.DgraphChannel{IsAdmin: 1}}
	inDM := where{conversation: "u1 u2"}

	if inChannel.topic(channelPoll) != ch.String() || inDM.topic(dmPoll) != "u1 u2" {
		t.Errorf("topics %q and %q", inChannel.topic(channelPoll), inDM.topic(dmPoll))
	}
	person := func(id uuid.UUID) *userModels.UserInfo {
		return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id}}
	}
	if !canClose(dmPoll, inDM, person(author)) || canClose(dmPoll, inDM, person(other)) {
		t.Error("only its author closes a conversation's poll")
	}
	if !canClose(channelPoll, inChannel, person(other)) {
		t.Error("a channel admin closes a channel's poll")
	}
	if v := view(dmPoll, &pollModel.Tally{Counts: map[string]int{}}, author, true); v.Channel != "" {
		t.Errorf("a conversation's poll names channel %q", v.Channel)
	}
}
