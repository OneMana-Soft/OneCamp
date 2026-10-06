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

func UpdateUserEmojiStatus(ctx context.Context, dgraphStatus *dgraphStruct.DgraphUserStatusEmoji) (err error) {
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(dgraphStatus)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateUserEmojiStatus failed to marshal dgraphUser struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson: pb,
	}
	req := &api.Request{
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateUserEmojiStatus failed to create or update dgraph status err: %+v",
			err)
		return
	}

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/UpdateUserEmojiStatus failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func CreateOrUpdateDgraphUser(ctx context.Context, dgraphUser *dgraphStruct.DgraphUser, query string, deleteJson string) (userUid string, err error) {
	pb, err := json.Marshal(dgraphUser)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphUser failed to marshal dgraphUser struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson:    pb,
		DeleteJson: []byte(deleteJson),
	}
	// An upsert, so a conflicting concurrent write is retried (DoCommitNow).
	res, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphUser failed to create or update dgraph user err: %+v",
			err)
		return
	}

	// will get uid only when new node is created
	userUid = res.Uids["uid(user)"]
	return
}

func GetDgraphUserInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphUserInfoByUUID failed to get user err: %+v",
			err)
		return
	}

	type Users struct {
		UserInfo []dgraphStruct.DgraphUser `json:"userInfo"`
	}

	var usersInfo Users
	err = json.Unmarshal(resp.Json, &usersInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphUserInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(usersInfo.UserInfo) == 0 {
		err = errors.New("failed to get dgraph user")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphUserInfoByUUID failed to get dgraph user")

		return
	}
	dgraphUser = &usersInfo.UserInfo[0]

	return
}

func GetDgraphUsersList(ctx context.Context, query string, variables map[string]string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphUsersList failed to get user err: %+v",
			err)
		return
	}

	type Users struct {
		UserInfo []*dgraphStruct.DgraphUser `json:"userInfo"`
	}

	var usersInfo Users
	err = json.Unmarshal(resp.Json, &usersInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphUsersList failed to unmarshal response json err: %+v",
			err)
		return
	}
	dgraphUsers = usersInfo.UserInfo
	return
}

func DeleteUserEdge(ctx context.Context, delStringJSON string) (err error) {
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
				"models/DeleteChannelEdge failed to discard dgraph txn err: %+v",
				err)
		}

	}()

	return nil
}

func ExecDgraphQuery(ctx context.Context, query string, setJson []byte) (err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	mu := &api.Mutation{
		SetJson: setJson,
	}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/ExecDgraphQuery failed to get user err: %+v",
			err)
		return
	}

	return
}
