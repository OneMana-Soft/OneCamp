package models

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

func CreateChat(ctx context.Context, query string, dgraphDm *dgraphStruct.DgraphDm) (chatUID string, dmUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChat failed to create or update dgraph dm err: %+v",
			err)
		return
	}

	chatUID = res.Uids["uid(ch)"]

	dmUID = res.Uids["uid(dm)"]

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateChat failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func UpdateChat(ctx context.Context, query string, dgraphChat *dgraphStruct.DgraphChat, delStringJSON string) (err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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
		CommitNow: true,
	}

	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChat failed to create or update dgraph dm err: %+v",
			err)
		return
	}

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/UpdateChat failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func GetDgraphChatInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphChats *dgraphStruct.DgraphChat, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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
		err = errors.New("failed to get dgraph chats")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChatInfoByGroupingID failed to get dgraph post")

		return
	}
	dgraphChats = &(chatsInfo.ChatInfo[0])

	return
}

func GetDgraphBasicDmsInfoByGrpId(ctx context.Context, query string, variables map[string]string) (dgraphDmInfo *dgraphStruct.DgraphDm, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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
	txn := dgraphInit.DgraphClient.NewTxn()

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

	return
}

func CreateOrUpdateChatReaction(ctx context.Context, query string, dgraphChat *dgraphStruct.DgraphChat) (reactionUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphChat)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateChatReaction failed to marshal dgraphChat struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson: pb,
	}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateChatReaction failed to create or update dgraph chat reaction err: %+v",
			err)
		return
	}

	for _, v := range res.Uids {
		reactionUID = v
	}

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateChatReaction failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func DeleteNodeOrEdgesRelatedToChats(ctx context.Context, delStringJSON string) (err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	mutation := &api.Mutation{
		DeleteJson: []byte(delStringJSON),
	}

	_, err = txn.Mutate(context.Background(), mutation)
	if err != nil {
		return err
	}

	err = txn.Commit(context.Background())
	if err != nil {
		return err
	}

	defer func() {
		err = txn.Discard(ctx)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/DeleteNodeOrEdgesRelatedToChats failed to discard dgraph txn err: %+v",
				err)
		}

	}()

	return nil
}

// BulkSoftDeleteDgraphChats sets chat_deleted_at on multiple chats in a single Dgraph mutation.
func BulkSoftDeleteDgraphChats(ctx context.Context, chats []*dgraphStruct.DgraphChat, query string) error {
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(chats)
	if err != nil {
		return err
	}
	mu := &api.Mutation{SetJson: pb}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}
	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphChats failed: %v", err)
		return err
	}
	return nil
}
