package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The channels and conversations a question names, for the AskAI context's
// one search of each (backend audit #20). A channel with no name named
// every question: strings.Contains(q, "") is true.
func TestWhatAQuestionNames(t *testing.T) {
	u := &userModels.UserInfo{}
	u.UserDgraphInfo.Channels = []*dgraphStruct.DgraphChannel{{Uuid: "c1", Name: "launch"}, {Uuid: "c2", Name: "design"}, {Uuid: "c3", Name: ""}}
	u.UserDgraphInfo.DMs = []*dgraphStruct.DgraphDm{
		{GroupingId: "g1", Participants: []*dgraphStruct.DgraphUser{{Uuid: "me", UserName: "Sam"}, {Uuid: "u-maya-0001", UserName: "Maya"}}},
		{GroupingId: "g2", Participants: []*dgraphStruct.DgraphUser{{Uuid: "me", UserName: "Sam"}, {Uuid: "u-bo-0002", UserName: "Bo"}}},
	}
	if got := namedChannels(u, "what happened in launch and design?"); len(got) != 2 || got[0] != "c1" || got[1] != "c2" {
		t.Errorf("channels: %v", got)
	}
	if got := namedChannels(u, "anything new?"); len(got) != 0 {
		t.Errorf("a question naming no channel named %v", got)
	}
	if got := namedConversations(u, "me", "what did maya say?"); len(got) != 1 || got[0] != "g1" {
		t.Errorf("conversations: %v", got)
	}
	if got := namedConversations(u, "me", "what did sam say?"); len(got) != 0 {
		t.Errorf("the reader named themself: %v", got)
	}
}
