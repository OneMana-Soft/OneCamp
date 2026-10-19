package domain

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// SetGroupName names the group groupingID (dm_name), or with "" takes its
// name away. Nothing is written when there is no such group.
func SetGroupName(ctx context.Context, groupingID, name string) error {
	mu := &api.Mutation{Cond: "@if(eq(len(dm), 1))"}
	if name == "" {
		mu.DelNquads = []byte(`uid(dm) <dm_name> * .`)
	} else {
		set, err := json.Marshal(map[string]string{"uid": "uid(dm)", "dm_name": name})
		if err != nil {
			return err
		}
		mu.SetJson = set
	}
	_, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     `query q($g: string) { dm as var(func: eq(dm_grouping_id, $g)) }`,
		Vars:      map[string]string{"$g": groupingID},
		Mutations: []*api.Mutation{mu},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetGroupName err: %+v", err)
	}
	return err
}
