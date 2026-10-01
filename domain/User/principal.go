package domain

import (
	"context"
	"fmt"

	models "github.com/akashc777/OneCamp/models/postgres/User"
)

// PrincipalByEmail assembles the pair every authenticated request carries: the
// Postgres row and the graph node for one person.
//
// Three callers were building it by hand and a fourth was about to, each with
// its own wording for the two ways it goes wrong. The distinction is worth
// keeping: somebody who exists in Postgres but not in the graph is a
// half-provisioned account, and reporting that as "no such user" sends whoever
// reads the message looking in the wrong place.
func PrincipalByEmail(ctx context.Context, email string) (*models.UserInfo, error) {
	if email == "" {
		return nil, fmt.Errorf("no email to resolve a principal from")
	}
	pg, err := GetUserByEmailId(ctx, &email)
	if err != nil {
		return nil, err
	}
	if pg == nil {
		return nil, fmt.Errorf("no such user: %s", email)
	}
	dg, err := GetDgraphUserInfoByUUID(ctx, pg.Id.String())
	if err != nil {
		return nil, err
	}
	if dg == nil {
		return nil, fmt.Errorf("%s exists in Postgres but not in the graph", email)
	}
	return &models.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *dg}, nil
}
