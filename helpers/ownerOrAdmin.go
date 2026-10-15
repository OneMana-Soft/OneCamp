package helpers

import "github.com/google/uuid"

// OwnerOrAdmin reports whether the person with userID may change something
// createdBy added: its creator, or a workspace admin. Something with no
// recorded creator is the admins' alone, and nobody signed in is never its
// creator.
//
// One rule for every admin-configured thing a member may also add (an agent,
// an MCP server), so "who may change this" has one answer.
func OwnerOrAdmin(createdBy *uuid.UUID, userID uuid.UUID, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	return createdBy != nil && *createdBy != uuid.Nil && userID != uuid.Nil && *createdBy == userID
}
