package business

import (
	"context"

	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// MayCreateProjects reports whether someone can start a project, and so add a
// template or have one drafted: a team's admin, or a workspace admin.
func MayCreateProjects(ctx context.Context, u *userModels.UserInfo) (bool, error) {
	if u.UserPostgresInfo.IsAdmin {
		return true, nil
	}
	teams, err := teamBusiness.GetDgraphTeamListByAdminDgraphUID(ctx, u.UserDgraphInfo.Uid)
	return len(teams) > 0, err
}
