package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestBuildUnreadSummary(t *testing.T) {
	me := "me-uuid"
	user := &dgraphStruct.DgraphUser{
		Channels: []*dgraphStruct.DgraphChannel{
			{Uuid: "c1", Name: "general", UnreadPostCount: 2},
			{Uuid: "c2", Name: "quiet"},
			{Uuid: "c3", Name: "launch", UnreadPostCount: 9},
		},
		DMs: []*dgraphStruct.DgraphDm{
			{GroupingId: "g1", UnreadMessageCount: 4, Participants: []*dgraphStruct.DgraphUser{{Uuid: me, UserName: "Me"}, {Uuid: "maya", UserName: "Maya"}}},
			{GroupingId: "g2", UnreadMessageCount: 1, Participants: []*dgraphStruct.DgraphUser{{Uuid: me}, {Uuid: "a", UserName: "Ana"}, {Uuid: "b", UserName: "Bo"}}},
		},
	}
	s := buildUnreadSummary(user, me, 3)
	if s.Channels != 11 || s.DMs != 5 || s.Activity != 3 || s.Total != 19 {
		t.Fatalf("counts: %+v", s)
	}
	if len(s.Top) != 4 || s.Top[0].Name != "#launch" || s.Top[0].Path != "/app/channel/c3" {
		t.Fatalf("busiest first, quiet channels left out: %+v", s.Top)
	}
	byName := map[string]UnreadPlace{}
	for _, p := range s.Top {
		byName[p.Name] = p
	}
	if p := byName["Maya"]; p.Kind != "dm" || p.Path != "/app/chat/maya" {
		t.Errorf("a one-to-one DM links by the other person: %+v", p)
	}
	if p := byName["Ana, Bo"]; p.Kind != "group" || p.Path != "/app/chat/group/g2" {
		t.Errorf("a group links by its grouping id: %+v", p)
	}
	if empty := buildUnreadSummary(nil, me, 0); empty.Total != 0 || empty.Top == nil {
		t.Errorf("nothing unread is zeros and an empty list, not null: %+v", empty)
	}
}
