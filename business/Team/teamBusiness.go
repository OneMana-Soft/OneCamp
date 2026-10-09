package business

import (
	"context"
	"errors"
	"time"

	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	domain "github.com/akashc777/OneCamp/domain/Team"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	models "github.com/akashc777/OneCamp/models/postgres/Team"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func CreateTeam(ctx context.Context, teamName string, userInfo *userModels.UserInfo) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	currentTime := time.Now()
	teamUUID := uuid.New()

	err = domain.CreateTeam(ctx, teamUUID, teamName, userInfo.UserPostgresInfo.Id, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateTeam failed to create team in postgress err: %+v", err)

		return
	}
	zeroUnixTime := time.Time{}

	dgraphTeam = &dgraphStruct.DgraphTeam{
		Uid:   "uid(team)",
		DType: []string{"Team"},
		Uuid:  teamUUID.String(),
		Name:  teamName,
		CreatedBy: &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		},
		Members: []*dgraphStruct.DgraphUser{{
			Uid: userInfo.UserDgraphInfo.Uid,
		}},
		Admins: []*dgraphStruct.DgraphUser{{
			Uid: userInfo.UserDgraphInfo.Uid,
			Teams: []*dgraphStruct.DgraphTeam{{
				Uid: "uid(team)",
			}},
		}},
		UpdatedAt: &currentTime,
		CreatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	teamUid, err := domain.CreateOrUpdateDgraphTeam(ctx, dgraphTeam)

	if helpers.DgraphWriteFailed(teamUid, err) {
		// Was logged as "failed to create team in postgress", copied from the branch above, so
		// a Dgraph failure sent whoever read the log to the wrong datastore.
		helpers.LogErrorWithContext(ctx, "business/CreateTeam failed to create team in dgraph err: %+v", err)

		// An empty uid with a nil error is still a failure, and this used to return that error
		// unchanged — so the caller received a fully populated dgraphTeam and err == nil for a
		// team that does not exist in Dgraph, and reported success to the user. Name the failure
		// instead of inheriting whatever err happened to be.
		if err == nil {
			err = errors.New("dgraph returned no uid for the new team")
		}

		// Reverse the Postgres insert. Leaving it made this PERMANENTLY UNREPEATABLE: teams.
		// team_name is UNIQUE and team listings read Dgraph (GetAllTeamDgraphInfo returns
		// DgraphTeam), so the row was a team nobody could see that still owned the name, and
		// every retry failed on the constraint.
		//
		// Safe here because projects — the only thing referencing teams(id) — cannot exist yet.
		_ = helpers.CompensateOnFailure(ctx, "team row for "+teamName,
			func(undoCtx context.Context) error {
				return domain.HardDeleteTeam(undoCtx, teamUUID)
			})

		return
	}

	go domain.CreateTeamInOpenSearch(&openSearchStruct.OpenSearchTeam{
		Uuid:          teamUUID.String(),
		TeamName:      teamName,
		TeamCreatedAt: currentTime.Unix(),
		TeamUpdatedAt: currentTime.Unix(),
		TeamDeletedAt: nil,
	})

	return
}

func CheckIfTeamExistByTeamName(ctx context.Context, teamName string) (exist bool, err error) {
	exist, err = domain.CheckIfTeamExistByTeamName(ctx, teamName)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CheckIfTeamExistByTeamName failed to check if team name exist in postgress err: %+v", err)

		return
	}

	return
}

func UpdateTeamName(ctx context.Context, teamName string, teamUUID uuid.UUID) (err error) {

	currentTime := time.Now()

	err = domain.UpdateTeamNameByTeamUUID(ctx, teamName, teamUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTeamName failed to update team name in postgress err: %+v", err)

		return
	}

	dgraphTeam := dgraphStruct.DgraphTeam{
		Uid:       "uid(team)",
		Uuid:      teamUUID.String(),
		Name:      teamName,
		UpdatedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphTeam(ctx, &dgraphTeam)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTeamName failed to update team name in dgraph err: %+v", err)

		return
	}

	go domain.UpdateTeamInOpenSearch(&openSearchStruct.OpenSearchTeam{
		Uuid:          teamUUID.String(),
		TeamName:      teamName,
		TeamUpdatedAt: currentTime.Unix(),
	})

	return
}

func AddMemberToTeam(ctx context.Context, teamUUID uuid.UUID, userDgraphUUID string) (err error) {

	dgraphTeam := dgraphStruct.DgraphTeam{
		Uid:  "uid(team)",
		Uuid: teamUUID.String(),
		Members: []*dgraphStruct.DgraphUser{{
			Uid: userDgraphUUID,
			Teams: []*dgraphStruct.DgraphTeam{{
				Uid: "uid(team)",
			}},
		}},
	}

	_, err = domain.CreateOrUpdateDgraphTeam(ctx, &dgraphTeam)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddMemberToTeam failed to add member to team in dgraph err: %+v", err)

		return
	}

	return
}

func AddAdminMemberToTeam(ctx context.Context, teamUUID uuid.UUID, userDgraphUUID string) (err error) {

	dgraphTeam := dgraphStruct.DgraphTeam{
		Uid:  "uid(team)",
		Uuid: teamUUID.String(),
		Admins: []*dgraphStruct.DgraphUser{{
			Uid: userDgraphUUID,
		}},
		Members: []*dgraphStruct.DgraphUser{{
			Uid: userDgraphUUID,
		}},
	}

	_, err = domain.CreateOrUpdateDgraphTeam(ctx, &dgraphTeam)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddAdminMemberToTeam failed to add admin member to team in dgraph err: %+v", err)

		return
	}

	return
}

func GetDgraphTeamListByAdminDgraphUID(ctx context.Context, userDgraphUID string) (dgraphTeamList []*dgraphStruct.DgraphTeam, err error) {
	dgraphTeamList, err = domain.GetDgraphTeamListByAdminDgraphUID(ctx, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphTeamListByAdminDgraphUID failed to get project list err: %+v", err)

		return
	}

	return
}

func GetDgraphTeamListByUserDgraphUID(ctx context.Context, userDgraphUID string) (dgraphTeamList []*dgraphStruct.DgraphTeam, err error) {
	dgraphTeamList, err = domain.GetDgraphTeamListByUserDgraphUID(ctx, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphTeamListByUserDgraphUID failed to get project list err: %+v", err)

		return
	}

	return
}

func RemoveAdminMemberFromTeam(ctx context.Context, teamDgraphUID string, userDgraphUID string) (err error) {

	err = domain.DeleteTeamAdminMemberEdge(ctx, teamDgraphUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveAdminMemberFromTeam failed to remove member from team in dgraph err: %+v", err)

		return
	}

	return

}

// RemoveMemberFromTeam takes someone (their graph uid, and their user uuid)
// out of a team and the team's projects.
func RemoveMemberFromTeam(ctx context.Context, teamDgraph *dgraphStruct.DgraphTeam, userDgraphUID string, memberUUID string) (err error) {

	err = domain.DeleteTeamMemberEdge(ctx, teamDgraph, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveMemberFromTeam failed to remove member from team in dgraph err: %+v", err)

		return
	}
	// Their cached profile still lists the team and its projects, and search
	// covers what it lists.
	userDomain.InvalidateUserMemberships(ctx, memberUUID)

	return

}

func ArchiveTeamByTeamUUID(ctx context.Context, teamUUID uuid.UUID, dgraphTeamInfo *dgraphStruct.DgraphTeam) (err error) {

	currentTime := time.Now()
	err = domain.UpdateTeamDeletedTimeByUUID(ctx, teamUUID, &currentTime, &currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveTeamByTeamUUID failed to add delete time in team in postgres err: %+v", err)

		return
	}

	dgraphTeam := &dgraphStruct.DgraphTeam{
		Uid:       "uid(team)",
		Uuid:      teamUUID.String(),
		DeletedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphTeam(ctx, dgraphTeam)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveTeamByTeamUUID failed to add delete time in team in dgraph err: %+v", err)

		return
	}

	go domain.UpdateTeamProjectsAndTasksDeleteTimeForOpenSearch(dgraphTeamInfo, currentTime.Unix())
	go domain.DeleteTeamInOpenSearch(teamUUID.String(), currentTime.Unix())
	// Cascade soft-delete to AI embeddings (all team content)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"team_uuid"},
		teamUUID.String(),
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)
	return
}

func UnArchiveTeamByTeamUUID(ctx context.Context, teamUUID uuid.UUID, dgraphTeamInfo *dgraphStruct.DgraphTeam) (err error) {

	currentTime := time.Now()
	zeroUnixTime := time.Time{}

	err = domain.UpdateTeamDeletedTimeToNullByUUID(ctx, teamUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnArchiveTeamByTeamUUID failed to remove delete time in team in postgress err: %+v", err)

		return
	}

	dgraphTeam := &dgraphStruct.DgraphTeam{
		Uid:       "uid(team)",
		Uuid:      teamUUID.String(),
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphTeam(ctx, dgraphTeam)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnArchiveTeamByTeamUUID failed to remove delete time in team in dgraph err: %+v", err)

		return
	}

	go domain.UpdateTeamProjectsAndTasksDeleteTimeForOpenSearch(dgraphTeamInfo, -1)
	go domain.DeleteTeamInOpenSearch(teamUUID.String(), 0)
	// Cascade unarchive to AI embeddings
	go globalSearchDomain.SyncCascadingUnarchiveInOpenSearch(
		[]string{"team_uuid"},
		teamUUID.String(),
		[]string{"ai_embeddings"},
		"cascade",
	)
	return
}

func GetTeamDgraphInfo(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {

	dgraphTeam, err = domain.GetDgraphTeamInfoByUUID(ctx, teamUUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetTeamDgraphInfo failed to get team info from dgraph err: %+v", err)
		return
	}

	return
}

func GetDgraphAllProjectListByTeamUUID(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	dgraphTeam, err = domain.GetDgraphAllProjectListByTeamUUID(ctx, teamUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphAllProjectListByTeamUUID failed to get team info from dgraph err: %+v", err)
		return
	}

	return
}

func GetDgraphUnDeletedProjectListByTeamUUID(ctx context.Context, teamUUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	dgraphTeam, err = domain.GetDgraphUnDeletedProjectListByTeamUUID(ctx, teamUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphUnDeletedProjectListByTeamUUID failed to get team info from dgraph err: %+v", err)
		return
	}

	return

}

func GetDgraphAllProjectListByTeamUUIDInWhichUserIsMember(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {

	dgraphTeam, err = domain.GetDgraphAllProjectListByTeamUUIDInWhichUserIsMember(ctx, teamUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphAllProjectListByTeamUUIDInWhichUserIsMember failed to get team info from dgraph err: %+v", err)
		return
	}

	return
}

func GetDgraphTeamMemberListByTeamUUID(ctx context.Context, teamUUID string, userDgraphUid string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	dgraphTeam, err = domain.GetDgraphTeamMemberListByTeamUUID(ctx, teamUUID, userDgraphUid)

	if err != nil || dgraphTeam == nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphTeamMemberListByTeamUUID failed to get team from dgraph err: %+v", err)
		return
	}

	adminIndex := 0
	adminLength := len(dgraphTeam.Admins)

	if adminLength > 0 {

		for _, member := range dgraphTeam.Members {
			if member.Uuid == dgraphTeam.Admins[adminIndex].Uuid {
				member.IsAdmin = true
				adminIndex = adminIndex + 1
			}

			if adminIndex == adminLength {
				break
			}
		}

	}

	dgraphTeam.Admins = nil

	return

}

func GetTeamPostgresInfo(ctx context.Context, teamUUID uuid.UUID) (teamInfo *models.Team, err error) {

	teamInfo, err = domain.GetTeamByUUID(ctx, teamUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetTeamPostgresInfo failed to get team info from postgres err: %+v", err)
		return
	}

	return
}

func GetBasicDgraphTeamInfoByUUID(ctx context.Context, teamUUID string, userDgraphUid string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	dgraphTeam, err = domain.GetBasicDgraphTeamInfoByUUID(ctx, teamUUID, userDgraphUid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBasicDgraphTeamInfoByUUID failed to get team info from dgraph err: %+v", err)
		return
	}

	return

}

func GetBasicDgraphTeamInfoAndMemberInfoByUUID(ctx context.Context, teamUUID string, userDgraphUid string, memberUUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {

	dgraphTeam, err = domain.GetBasicDgraphTeamInfoAndMemberInfoByUUID(ctx, teamUUID, userDgraphUid, memberUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBasicDgraphTeamInfoAndMemberInfoByUUID failed to get team info from dgraph err: %+v", err)
		return
	}

	return

}

func GetBasicDgraphTeamInfoAndAdminMemberInfoByUUID(ctx context.Context, teamUUID string, userDgraphUid string, memberUUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	dgraphTeam, err = domain.GetBasicDgraphTeamInfoAndAdminMemberInfoByUUID(ctx, teamUUID, userDgraphUid, memberUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBasicDgraphTeamInfoAndAdminMemberInfoByUUID failed to get team info from dgraph err: %+v", err)
		return
	}

	return
}

func GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam(ctx context.Context, teamUUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	dgraphTeam, err = domain.GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam(ctx, teamUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam failed to get team info from dgraph err: %+v", err)
		return
	}

	return
}

func GetAllTeamDgraphInfo(ctx context.Context, userDgraphUid string, pageIndex int, pageSize int) (dgraphTeam []*dgraphStruct.DgraphTeam, hasMore bool, err error) {
	dgraphTeam, actualLen, err := domain.GetAllTeamDgraphInfo(ctx, userDgraphUid, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetAllTeamDgraphInfo failed to get team list from dgraph err: %+v", err)
		return
	}

	if actualLen > pageSize {
		hasMore = true
		dgraphTeam = dgraphTeam[:pageSize]
	}

	return
}
