package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// SetCallFactsByUUID writes a call's message's facts (chat_system) and
// nothing else, by its uuid, in one request: the message gains its end and
// length when the call ends (calls CALLS-3b, business/Huddle), never through
// the edit path. Only a message of kind huddle is ever written.
func SetCallFactsByUUID(ctx context.Context, chatUUID, system string) error {
	set, err := json.Marshal(map[string]string{"uid": "uid(c)", "chat_system": system})
	if err != nil {
		return err
	}
	_, err = dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     `query q($id: string) { c as var(func: eq(chat_uuid, $id)) @filter(eq(chat_kind, "huddle")) }`,
		Vars:      map[string]string{"$id": chatUUID},
		Mutations: []*api.Mutation{{Cond: `@if(eq(len(c), 1))`, SetJson: set}},
	})
	return err
}
