package models

import "github.com/akashc777/OneCamp/helpers"

// DisplayName is the name people see for this member, by the one rule
// (helpers.PersonDisplayName): display name, else full name, else the
// address's part before the @. "" for a nil user.
func (u *DgraphUser) DisplayName() string {
	if u == nil {
		return ""
	}
	return helpers.PersonDisplayName(u.UserName, u.UserFullName, u.EmailID)
}
