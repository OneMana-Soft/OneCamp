package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// ErrNotFound is a person the graph has no node for, as opposed to a graph
// that didn't answer: a read of them answers 404 (helpers.WriteReadFailure).
var ErrNotFound = helpers.NotFound("failed to get dgraph user")

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

func GetDgraphUserInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()
	return GetDgraphUserInfoInTxn(ctx, txn, query, variables)
}

// GetDgraphUserInfoInTxn is GetDgraphUserInfoByUUID in txn, for a read made
// of several queries that must see one snapshot (dgraphInit.InSnapshot).
func GetDgraphUserInfoInTxn(ctx context.Context, txn *dgo.Txn, query string, variables map[string]string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
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
		err = ErrNotFound
		// Found nothing: an answer its caller handles (a 404, or skipping
		// the thing), so said at DEBUG, not as a failure.
		helpers.LogDebugWithContext(ctx,
			"models/GetDgraphUserInfoByUUID failed to get dgraph user")

		return
	}
	dgraphUser = &usersInfo.UserInfo[0]
	// Only the tasks the reader may see (models/taskread).
	if err = taskread.KeepIn(ctx, dgraphUser); err != nil {
		return nil, err
	}
	return
}

func GetDgraphUsersList(ctx context.Context, query string, variables map[string]string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

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
	if err = taskread.KeepIn(ctx, &dgraphUsers); err != nil {
		return nil, err
	}
	return
}

func DeleteUserEdge(ctx context.Context, delStringJSON string) (err error) {
	_, err = dgraphInit.DoCommitNow(ctx, &api.Request{
		Mutations: []*api.Mutation{{DeleteJson: []byte(delStringJSON)}},
	})
	return err
}
