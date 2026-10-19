package domain

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// LeaveGroup takes the person userUID names out of the group groupingID: both
// edges between them (dm_participants and user_dms). Nothing is written when
// there is no such group; a uid that isn't one is refused before anything is.
func LeaveGroup(ctx context.Context, groupingID, userUID string) error {
	if !dgraphquery.AllUIDs([]string{userUID}) {
		return fmt.Errorf("domain/LeaveGroup: %q isn't a uid", userUID)
	}
	_, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: `query q($g: string) { dm as var(func: eq(dm_grouping_id, $g)) }`,
		Vars:  map[string]string{"$g": groupingID},
		Mutations: []*api.Mutation{{
			Cond: "@if(eq(len(dm), 1))",
			DelNquads: []byte(fmt.Sprintf("uid(dm) <dm_participants> <%s> .\n<%s> <user_dms> uid(dm) .",
				userUID, userUID)),
		}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/LeaveGroup err: %+v", err)
	}
	return err
}
