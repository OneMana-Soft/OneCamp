package domain

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/User"
	"github.com/google/uuid"
)

// GetPeopleByUUIDs reads what an inbox item shows of the people in it: their
// names, picture and whether they're a bot, by uuid, in one query, with the
// email that tells which kind of bot one is (the caller never shows it). Ids
// that aren't uuids are left out. Deleted people are read too: an item from
// someone who has since left still says who it was.
func GetPeopleByUUIDs(ctx context.Context, userUUIDs []string) ([]*dgraphStruct.DgraphUser, error) {
	var quoted []string
	seen := map[string]bool{}
	for _, id := range userUUIDs {
		if u, err := uuid.Parse(id); err == nil && !seen[u.String()] {
			seen[u.String()] = true
			quoted = append(quoted, `"`+u.String()+`"`)
		}
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	people, err := dgraphModels.GetDgraphUsersList(ctx, `query people {
			userInfo(func: eq(user_uuid, [`+strings.Join(quoted, ", ")+`])) {
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
