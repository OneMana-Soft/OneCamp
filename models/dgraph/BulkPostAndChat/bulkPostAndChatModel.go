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

	data := struct {
		Chats []*dgraphStruct.DgraphDm   `json:"chats,omitempty"`
		Posts []*dgraphStruct.DgraphPost `json:"posts,omitempty"`
	}{
		Chats: dgraphDMs,
		Posts: dgraphPosts,
	}

	// Convert data to JSON
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(data)
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

	// userUid = res.Uids["uid(attachment)"]

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/BulkAddAttachmentsToDgraph failed to discard dgraph txn err: %+v",
				err)
		}
	}()
	return
}
