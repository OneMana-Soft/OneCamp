package business

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
)

// HasMember reports whether the person with userUUID is in the project, as a
// member or one of its admins (who may see its tasks; see Task.CanViewTask).
// Someone who isn't an active person is in nothing.
func HasMember(ctx context.Context, userUUID, projectID string) (bool, error) {
	user, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil || user == nil || user.Uid == "" {
		return false, err
	}
	p, err := domain.GetBasicDgraphProjectInfo(ctx, projectID, user.Uid)
	if err != nil {
		return false, err
	}
	return p != nil && p.Uuid == projectID && (p.IsProjectMember > 0 || p.IsProjectAdmin > 0), nil
}

// MembersAmong is which of these people are in the project (members or
// admins, still active), read at once: HasMember for a list.
func MembersAmong(ctx context.Context, projectID string, userUUIDs []string) (map[string]bool, error) {
	return domain.MembersAmong(ctx, projectID, userUUIDs)
}

// MembersAmongEach is MembersAmong for many projects at once, keyed by
// project as asked: a read a page of projects, not one per project.
func MembersAmongEach(ctx context.Context, asked map[string][]string) (map[string]map[string]bool, error) {
	return domain.MembersAmongEach(ctx, asked)
}
