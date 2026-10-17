package models

import (
	"sort"

	"github.com/akashc777/OneCamp/helpers"
)

// DisplayName is the name people see for this member, by the one rule
// (helpers.PersonDisplayName): display name, else full name, else the
// address's part before the @. "" for a nil user.
func (u *DgraphUser) DisplayName() string {
	if u == nil {
		return ""
	}
	return helpers.PersonDisplayName(u.UserName, u.UserFullName, u.EmailID)
}

// SortUsersByName orders people by user_name as Dgraph's orderasc does (byte
// by byte, anyone without one last), then by uuid. Member lists are sorted
// here rather than in the query: Dgraph stops a sorted block at 1,000
// without saying so, and a workspace can be bigger than that.
func SortUsersByName(users []*DgraphUser) {
	sort.SliceStable(users, func(i, j int) bool {
		a, b := users[i], users[j]
		switch {
		case a == nil || b == nil:
			return a != nil && b == nil
		case (a.UserName == "") != (b.UserName == ""):
			return b.UserName == ""
		case a.UserName != b.UserName:
			return a.UserName < b.UserName
		}
		return a.Uuid < b.Uuid
	})
}
