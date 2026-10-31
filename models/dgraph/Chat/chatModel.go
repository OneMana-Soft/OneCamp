package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

func CreateChat(ctx context.Context, query string, dgraphDm *dgraphStruct.DgraphDm) (chatUID string, dmUID string, err error) {
	pb, err := json.Marshal(dgraphDm)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChat failed to marshal dgraphDm struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson: pb,
	}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
	}
	dgraphStruct.StampLastChats(req, dgraphDm)

	res, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChat failed to create or update dgraph dm err: %+v",
			err)
		return
	}

	chatUID = res.Uids["uid(ch)"]

	dmUID = res.Uids["uid(dm)"]

	return
}

func UpdateChat(ctx context.Context, query string, dgraphChat *dgraphStruct.DgraphChat, delStringJSON string) (err error) {
	pb, err := json.Marshal(dgraphChat)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChat failed to marshal dgraphDm struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson:    pb,
		DeleteJson: []byte(delStringJSON),
	}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
	}

	_, err = dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChat failed to create or update dgraph dm err: %+v",
			err)
		return
	}

	return
}

// ErrNotFound is a message that isn't in the graph: an answer about the
// message, where any other error is the graph not answering. It is also
// helpers.ErrNotFound, which helpers.WriteReadFailure answers as a 404.
var ErrNotFound = helpers.NotFound("failed to get dgraph chats")

func GetDgraphChatInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphChats *dgraphStruct.DgraphChat, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChatInfoByUUID failed to get chat err: %+v",
			err)
		return
	}

	type Chats struct {
		ChatInfo []dgraphStruct.DgraphChat `json:"chatInfo"`
	}

	var chatsInfo Chats
	err = json.Unmarshal(resp.Json, &chatsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChatInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(chatsInfo.ChatInfo) == 0 {
		err = ErrNotFound
		// Found nothing: an answer its caller handles (a 404, or skipping
		// the thing), so said at DEBUG, not as a failure.
		helpers.LogDebugWithContext(ctx,
			"models/GetDgraphChatInfoByGroupingID failed to get dgraph post")

		return
	}
	dgraphChats = &(chatsInfo.ChatInfo[0])
	// A forwarded original deleted since shows as deleted, never its text.
	dgraphChats.HideDeletedForwards()

	return
}

func GetDgraphBasicDmsInfoByGrpId(ctx context.Context, query string, variables map[string]string) (dgraphDmInfo *dgraphStruct.DgraphDm, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphBasicDmsInfoByGrpId failed to get chat err: %+v",
			err)
		return
	}

	type Dms struct {
		DmInfo []dgraphStruct.DgraphDm `json:"dmInfo"`
	}

	var dmsInfo Dms
	err = json.Unmarshal(resp.Json, &dmsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphBasicDmsInfoByGrpId failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(dmsInfo.DmInfo) == 0 {
		return
	}
	dgraphDmInfo = &dmsInfo.DmInfo[0]

	return
}

func GetDgraphDmsInfoByGrpId(ctx context.Context, query string, variables map[string]string) (dgraphChats []*dgraphStruct.DgraphChat, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDmsInfoByGrpId failed to get chat err: %+v",
			err)
		return
	}

	type Dms struct {
		DmInfo []dgraphStruct.DgraphDm `json:"dmInfo"`
	}

	var dmsInfo Dms
	err = json.Unmarshal(resp.Json, &dmsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDmsInfoByGrpId failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(dmsInfo.DmInfo) == 0 {
		return
	}
	dgraphChats = dmsInfo.DmInfo[0].Chats
	// A forwarded original deleted since shows as deleted, never its text.
	for _, c := range dgraphChats {
		c.HideDeletedForwards()
	}

	return
}

// BulkSoftDeleteDgraphChats sets chat_deleted_at on multiple chats in a single Dgraph mutation.
func BulkSoftDeleteDgraphChats(ctx context.Context, chats []*dgraphStruct.DgraphChat, query string) error {
	pb, err := json.Marshal(chats)
	if err != nil {
		return err
	}
	mu := &api.Mutation{SetJson: pb}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
	}
	_, err = dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphChats failed: %v", err)
		return err
	}
	return nil
}
