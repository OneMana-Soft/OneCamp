package business

import (
	"context"
	"slices"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The "@" list's server part: who is here, recent writers first and only
// those still here, the writer's teams with how many of each are here, and
// the broadcasts with why they're off in a thread or by a rule.
func TestWhatTheAtListIsTold(t *testing.T) {
	w := &world{members: []string{maya, jonas, sam}, membersOK: true}
	stub(t, w)
	oldRecent, oldTeams, oldZones, oldDefault := recentWriters, teamsToOffer, zonesOf, defaultChannel
	t.Cleanup(func() {
		recentWriters, teamsToOffer, zonesOf, defaultChannel = oldRecent, oldTeams, oldZones, oldDefault
	})
	recentWriters = func(context.Context, string, string, int) ([]string, error) { return []string{sam, zed, maya}, nil }
	teamsToOffer = func(context.Context, *userModels.UserInfo) ([]*dgraphStruct.DgraphTeam, error) {
		return []*dgraphStruct.DgraphTeam{
			{Uuid: "t-2", Name: "eng", Members: []*dgraphStruct.DgraphUser{{Uuid: jonas}, {Uuid: maya}, {Uuid: zed}}},
			{Uuid: "t-1", Name: "Design", Members: []*dgraphStruct.DgraphUser{{Uuid: sam}}},
		}, nil
	}
	zonesOf = func(context.Context, []string) (map[string]string, error) {
		return map[string]string{jonas: "Europe/Berlin"}, nil
	}
	defaultChannel = func(string) bool { return false }
	me := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0xa1", Uuid: maya}}
	ch := &dgraphStruct.DgraphChannel{Uuid: "c-1", Name: "launch", MemberCount: 3}
	ctx := context.Background()

	m, err := MentionablesIn(ctx, me, Place{Kind: "channel", ID: "c-1", Channel: ch})
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 3 || !slices.Equal(m.Recent, []string{sam, maya}) || m.Zones[jonas] != "Europe/Berlin" {
		t.Errorf("count %d, recent %v, zones %v", m.Count, m.Recent, m.Zones)
	}
	if len(m.Teams) != 2 || m.Teams[0].Name != "Design" || m.Teams[0].Here != 1 || m.Teams[1].Here != 1 || m.Teams[1].Members != 3 {
		t.Errorf("teams %+v: Design first, each with one here but Maya", m.Teams)
	}
	if len(m.Broadcasts) != 2 || m.Broadcasts[1].Name != Channel || m.Broadcasts[1].Count != 2 || m.Why != "" {
		t.Errorf("broadcasts %+v why %q", m.Broadcasts, m.Why)
	}
	if m, _ := MentionablesIn(ctx, me, Place{Kind: "channel", ID: "c-1", Channel: ch, Thread: true}); m.Why != threadWhy {
		t.Errorf("in a thread: %q", m.Why)
	}
	w.membersOK = false
	if m, _ := MentionablesIn(ctx, me, Place{Kind: "channel", ID: "c-1", Channel: ch}); m.Why != "Only #launch's managers can notify everyone in it." {
		t.Errorf("where members may not: %q", m.Why)
	}
	if m, _ := MentionablesIn(ctx, me, Place{Kind: "group", ID: "g-1", People: []string{maya, sam}}); len(m.Broadcasts) != 0 || m.Count != 2 {
		t.Errorf("a group: %+v", m)
	}
}
