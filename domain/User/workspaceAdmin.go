package domain

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// IsWorkspaceAdmin reports whether a member (by their uuid) is one of the
// workspace's admins now, as far as this call goes: an admin's powers never
// reach a call an API token made (helpers.ActsThroughToken). Unknown, or a
// database that doesn't answer, is no. Calls (who hosts) and boards (who may
// protect an object) ask it. An admin is a live member whose email is in
// admin_users, as everywhere else (GetActiveUserWithAdminFlagByUserUUID):
// GetUserByUUID never reads that, so through it every admin was a no.
func IsWorkspaceAdmin(ctx context.Context, userUUID string) bool {
	if helpers.ActsThroughToken(ctx) {
		return false
	}
	id, err := uuid.Parse(strings.TrimSpace(userUUID))
	if err != nil {
		return false
	}
	u, err := GetActiveUserWithAdminFlagByUserUUID(ctx, id)
	return err == nil && u != nil && u.IsAdmin
}
