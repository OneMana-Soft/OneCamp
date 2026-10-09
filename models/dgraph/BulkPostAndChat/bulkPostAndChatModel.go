package model

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

func BulkAddChatAndPostToDgraph(ctx context.Context, query string, dgraphPosts []*dgraphStruct.DgraphPost, dgraphDMs []*dgraphStruct.DgraphDm) (dgraphChaatAndPostUUID []string, err error) {

	// One list of the nodes to write. Wrapping them in an object made a node of
	// its own, holding "chats" and "posts", on every forward.
	nodes := make([]interface{}, 0, len(dgraphDMs)+len(dgraphPosts))
	for _, dm := range dgraphDMs {
		nodes = append(nodes, dm)
	}
	for _, p := range dgraphPosts {
		nodes = append(nodes, p)
	}

	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() { _ = txn.Discard(ctx) }()
	pb, err := json.Marshal(nodes)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkAddChatAndPostToDgraph failed to marshal dgraphChatsAndPosts struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson: pb,
	}
	req := &api.Request{
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
		Query:     query,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkAddChatAndPostToDgraph failed to bulk create dgraph chats and posts err: %+v",
			err)
		return
	}

	for _, dgraphUid := range res.Uids {

		dgraphChaatAndPostUUID = append(dgraphChaatAndPostUUID, dgraphUid)
	}

	return
}
