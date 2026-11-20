package business

import (
	"context"
	"errors"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// CR-445 F2: MayMakeProjectIn is the one rule the project's own create, the
// AI's and a table template's install ask, so it is tested here, not only
// through their seams. With the workspace's "Who can make projects" at its
// default (the team's admins; no store in a unit test reads as the default),
// someone outside the team, a member and anyone in a deleted team may not; the
// team's admin may. The team is read for the team asked about and for them.
func TestOnlyATeamsAdminMakesProjectsInItByDefault(t *testing.T) {
	old := readTeamFor
	t.Cleanup(func() { readTeamFor = old })
	gone := time.Now().Add(-time.Hour)
	who := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x2a"}}
	for _, c := range []struct {
		name string
		team dgraphStruct.DgraphTeam
		may  bool
	}{
		{"not in the team", dgraphStruct.DgraphTeam{}, false},
		{"a member, admins making them", dgraphStruct.DgraphTeam{IsMember: 1}, false},
		{"its admin, but the team is deleted", dgraphStruct.DgraphTeam{IsAdmin: 1, IsMember: 1, DeletedAt: &gone}, false},
		{"its admin", dgraphStruct.DgraphTeam{IsAdmin: 1, IsMember: 1}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var askedTeam, askedFor string
			readTeamFor = func(_ context.Context, teamUUID, userUid string) (*dgraphStruct.DgraphTeam, error) {
				askedTeam, askedFor = teamUUID, userUid
				team := c.team
				team.Uuid = teamUUID
				return &team, nil
			}
			team, may, err := MayMakeProjectIn(context.Background(), "team-1", who)
			if err != nil || team == nil || team.Uuid != "team-1" {
				t.Fatalf("got team %v, err %v; want team-1 and no error", team, err)
			}
			if may != c.may {
				t.Errorf("may = %v, want %v", may, c.may)
			}
			if askedTeam != "team-1" || askedFor != "0x2a" {
				t.Errorf("read team %q for %q, want team-1 for 0x2a", askedTeam, askedFor)
			}
		})
	}
}

// A team that couldn't be read is an error, never a yes.
func TestATeamThatCannotBeReadMakesNoProject(t *testing.T) {
	old := readTeamFor
	t.Cleanup(func() { readTeamFor = old })
	down := errors.New("dgraph: unavailable")
	readTeamFor = func(context.Context, string, string) (*dgraphStruct.DgraphTeam, error) { return nil, down }
	team, may, err := MayMakeProjectIn(context.Background(), "team-1", &userModels.UserInfo{})
	if !errors.Is(err, down) || may || team != nil {
		t.Fatalf("got team %v, may %v, err %v; want no team, no and the read's error", team, may, err)
	}
}
