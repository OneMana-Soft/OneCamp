package domain

import (
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// DemoVisibleUsers returns users without the people a list shown to
// viewerEmail leaves out on the public demo (helpers.DemoListsPerson): for the
// demo's shared visitor, only the demo's own people and bots; for anyone
// else, users as it is. Every list the visitor picks people from (the
// @mention popup, the people it can message, the add-member pickers, user
// search) passes through here.
func DemoVisibleUsers(viewerEmail string, users []*dgraphStruct.DgraphUser) []*dgraphStruct.DgraphUser {
	return helpers.DemoPeopleOnly(viewerEmail, users,
		func(u *dgraphStruct.DgraphUser) string {
			if u == nil {
				return ""
			}
			return u.EmailID
		},
		func(u *dgraphStruct.DgraphUser) bool { return u != nil && u.IsBot })
}
