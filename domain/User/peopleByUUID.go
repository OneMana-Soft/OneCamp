package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/User"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
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

// NameForGuests is how a member is named to people outside the workspace (a
// meeting's guests, a room's public address), never an address
// (MemberName): their users.display_name when one is set (written for bots,
// imported and provisioned people), else the name they go by in the app, as
// a call's own tiles show it (the graph's). "" when there is neither, or
// neither can be read. The admin screens' read of display_name falls back to
// the address, and most members have none, so that read alone named them by
// their email.
func NameForGuests(ctx context.Context, id uuid.UUID) string {
	if names, err := models.DisplayNamesByUUIDs(ctx, []uuid.UUID{id}); err == nil {
		if n := MemberName(names[id]); n != "" {
			return n
		}
	}
	people, err := GetPeopleByUUIDs(ctx, []string{id.String()})
	if err != nil {
		return ""
	}
	// The person's own node comes first; an import's later copies may hold no name.
	for _, p := range people {
		if p == nil {
			continue
		}
		if n := MemberName(p.UserName, p.UserFullName); n != "" {
			return n
		}
	}
	return ""
}

// UserUIDsByUUIDs is each person's own graph node, by uuid: the first one
// (lowest uid) holding their user_uuid, which every read of a person by uuid
// takes, made when they joined. Imports made one more node with the person's
// user_uuid for each file they kept, and the person's next write of their own
// fields types those as a User too; they come after it.
func UserUIDsByUUIDs(ctx context.Context, userUUIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	list := dgraphquery.UUIDListOf(userUUIDs)
	if list == "" {
		return nil, nil
	}
	got, err := dgexec.Read[struct {
		People []struct {
			Uid  string    `json:"uid"`
			Uuid uuid.UUID `json:"user_uuid"`
		} `json:"people"`
	}](ctx, `query people {
			people(func: eq(user_uuid, [`+list+`])) { uid user_uuid }
		}`, nil)
	if err != nil {
		return nil, err
	}
	uids := make(map[uuid.UUID]string, len(got.People))
	for _, p := range got.People {
		if _, seen := uids[p.Uuid]; !seen {
			uids[p.Uuid] = p.Uid
		}
	}
	return uids, nil
}
