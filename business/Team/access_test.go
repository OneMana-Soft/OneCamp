package business

import (
	"context"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkDomain "github.com/akashc777/OneCamp/domain/EntityLink"
)

// A team is as CanReadTeam has it, through the Access Resolver: its admins
// run it, its members work in it, anyone else (a workspace admin too) learns
// nothing, and an archived one is read alone.
func TestATeamIsDecidedByItsOwnRule(t *testing.T) {
	gone := time.Now()
	prev := readTeamStandings
	t.Cleanup(func() { readTeamStandings = prev })
	readTeamStandings = func(_ context.Context, ids []string, uid string) ([]entityLinkDomain.TeamChip, error) {
		stand := map[string]entityLinkDomain.TeamChip{
			"0x1": {UUID: "a0000000-0000-0000-0000-00000000000a", Admin: 1, Member: 1},
			"0x2": {UUID: "a0000000-0000-0000-0000-00000000000a", Member: 1},
			"0x3": {UUID: "a0000000-0000-0000-0000-00000000000a"},
			"0x4": {UUID: "a0000000-0000-0000-0000-00000000000a", Member: 1, DeletedAt: &gone},
		}
		return []entityLinkDomain.TeamChip{stand[uid]}, nil
	}
	team := "A0000000-0000-0000-0000-00000000000A"
	for _, c := range []struct {
		who  accessBusiness.Subject
		want accessBusiness.Level
	}{
		{accessBusiness.Subject{UserUID: "0x1"}, accessBusiness.LevelFull},
		{accessBusiness.Subject{UserUID: "0x2"}, accessBusiness.LevelEdit},
		{accessBusiness.Subject{UserUID: "0x3", Admin: true}, accessBusiness.LevelNone},
		{accessBusiness.Subject{UserUID: "0x4"}, accessBusiness.LevelView},
		{accessBusiness.Subject{}, accessBusiness.LevelNone},
	} {
		got, err := accessBusiness.ResolveAll(context.Background(), c.who, []accessBusiness.Resource{{Kind: KindTeam, UUID: team}})
		if err != nil {
			t.Fatal(err)
		}
		if d := got[accessBusiness.Resource{Kind: KindTeam, UUID: team}]; d.Level != c.want {
			t.Errorf("%s: %+v, want %v", c.who.UserUID, d, c.want)
		}
	}
}
