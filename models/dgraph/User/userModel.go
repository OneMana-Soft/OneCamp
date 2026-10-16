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

// UpsertEmojiStatuses writes a person's statuses in one transaction: query
// names them (by var), set and del say what to write and what to remove (del
// may be nil). A conflicting concurrent write is retried (DoCommitNow).
func UpsertEmojiStatuses(ctx context.Context, query string, set, del []byte) error {
	_, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{SetJson: set, DeleteJson: del}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpsertEmojiStatuses failed to write the statuses err: %+v", err)
	}
	return err
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
