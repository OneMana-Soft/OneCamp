package business

import dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"

// CanReadTeam is who may read a team: a member, a team admin or a workspace
// admin. team is the team as read for them (its team_is_member and
// team_is_admin counted against their graph uid); nil, a team that wasn't
// found, is read by no one. Archive is not its business: the team's own
// pages read an archived team, and a caller that refuses one checks it apart.
// The team's info and members pages and the board list's team filter ask
// this one question.
func CanReadTeam(team *dgraphStruct.DgraphTeam, workspaceAdmin bool) bool {
	return team != nil && (team.IsMember > 0 || team.IsAdmin > 0 || workspaceAdmin)
}
