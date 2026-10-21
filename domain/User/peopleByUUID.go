package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/User"
)

// GetPeopleByUUIDs reads what an inbox item shows of the people in it: their
// names, picture and whether they're a bot, by uuid, in one query, with the
// email that tells which kind of bot one is (the caller never shows it). Ids
// that aren't uuids are left out. Deleted people are read too: an item from
// someone who has since left still says who it was.
func GetPeopleByUUIDs(ctx context.Context, userUUIDs []string) ([]*dgraphStruct.DgraphUser, error) {
	list := dgraphquery.UUIDList(userUUIDs)
	if list == "" {
		return nil, nil
	}
	people, err := dgraphModels.GetDgraphUsersList(ctx, `query people {
			userInfo(func: eq(user_uuid, [`+list+`])) {
				uid
				user_uuid
				user_name
				user_full_name
				user_profile_object_key
				is_bot
				user_email_id
			}
		}`, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetPeopleByUUIDs err: %+v", err)
	}
	return people, err
}
