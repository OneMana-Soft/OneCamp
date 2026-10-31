package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// SetCallFactsByUUID writes a call's post's facts (post_system) and nothing
// else, by its uuid, in one request: the post gains its end and length when
// the call ends (calls CALLS-3b, business/Huddle), never through the edit
// path. Only a post of kind huddle is ever written: an ordinary message or a
// system line keeps its own, whoever asks.
func SetCallFactsByUUID(ctx context.Context, postUUID, system string) error {
	set, err := json.Marshal(map[string]string{"uid": "uid(p)", "post_system": system})
	if err != nil {
		return err
	}
	_, err = dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     `query q($id: string) { p as var(func: eq(post_uuid, $id)) @filter(eq(post_kind, "huddle")) }`,
		Vars:      map[string]string{"$id": postUUID},
		Mutations: []*api.Mutation{{Cond: `@if(eq(len(p), 1))`, SetJson: set}},
	})
	return err
}
