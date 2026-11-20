package business

import (
	"context"

	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// readTeamFor is the team as its reader stands in it (IsAdmin, IsMember,
// DeletedAt): a seam, so the rule below is tested without Dgraph.
var readTeamFor = teamBusiness.GetBasicDgraphTeamInfoByUUID

// MayMakeProjectIn is the team (read for who) and whether who may make
// projects in it, by the workspace's "Who can make projects"
// (workAccess.CanMakeProjects; through an API token, as one of the team's
// members). Every path that makes a project for someone asks it first: the
// project's own create and a table template's install. err is a team that
// couldn't be read; one they aren't in reads as may false.
func MayMakeProjectIn(ctx context.Context, teamUUID string, who *userModels.UserInfo) (team *dgraphStruct.DgraphTeam, may bool, err error) {
	team, err = readTeamFor(ctx, teamUUID, who.UserDgraphInfo.Uid)
	if err != nil {
		return nil, false, err
	}
	return team, workAccess.CanMakeProjects(ctx, team), nil
}
