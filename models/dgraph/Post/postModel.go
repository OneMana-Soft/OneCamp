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

func CreateOrUpdatePost(ctx context.Context, query string, dgraphPost *dgraphStruct.DgraphPost, delStringJSON string) (postUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphPost)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdatePost failed to marshal dgraphPost struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson: pb,
	}
	if len(delStringJSON) > 0 {
		mu.DeleteJson = []byte(delStringJSON)
	}

	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdatePost failed to create or update dgraph post err: %+v",
			err)
		return
	}

	postUID = res.Uids["uid(po)"]
	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdatePost failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func CreateOrUpdatePostReaction(ctx context.Context, query string, dgraphPost *dgraphStruct.DgraphPost) (reactionUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphPost)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdatePostReaction failed to marshal dgraphPost struct err: %+v",
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
			"models/CreateOrUpdatePostReactiont failed to create or update dgraph post reaction err: %+v",
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
				"models/CreateOrUpdatePostReaction failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func GetDgraphPostInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphUser *dgraphStruct.DgraphPost, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostInfoByUUID failed to get post err: %+v",
			err)
		return
	}

	type Posts struct {
		PostInfo []dgraphStruct.DgraphPost `json:"postInfo"`
	}

	var postsInfo Posts
	err = json.Unmarshal(resp.Json, &postsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(postsInfo.PostInfo) == 0 {
		err = errors.New("failed to get dgraph user")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostInfoByUUID failed to get dgraph post")

		return
	}
	dgraphUser = &postsInfo.PostInfo[0]

	return
}

func GetDgraphPosts(ctx context.Context, query string, variables map[string]string) (dgraphPost []*dgraphStruct.DgraphPost, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPosts failed to get post err: %+v",
			err)
		return
	}

	type Posts struct {
		PostInfo []dgraphStruct.DgraphChannel `json:"postInfo"`
	}

	var postsInfo Posts
	err = json.Unmarshal(resp.Json, &postsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPosts failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(postsInfo.PostInfo) == 0 {
		return
	}
	dgraphPost = postsInfo.PostInfo[0].Posts

	return
}

func DeleteNodeOrEdges(ctx context.Context, delStringJSON string) (err error) {
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
				"models/DeleteNodeOrEdges failed to discard dgraph txn err: %+v",
				err)
		}

	}()

	return nil
}

// BulkSoftDeleteDgraphPosts sets post_deleted_at on multiple posts in a single Dgraph mutation.
func BulkSoftDeleteDgraphPosts(ctx context.Context, posts []*dgraphStruct.DgraphPost, query string) error {
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(posts)
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
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphPosts failed: %v", err)
		return err
	}
	return nil
}

// DeleteByUpsert deletes the nodes a query names: the delete JSON refers to
// the query's variables ("uid(a)"), so the lookup and the delete happen in one
// request. The sibling of DeleteNodeOrEdges for callers that hold ids, not uids.
func DeleteByUpsert(ctx context.Context, query string, delJSON string) error {
	txn := dgraphInit.DgraphClient.NewTxn()
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{DeleteJson: []byte(delJSON)}},
		CommitNow: true,
	}
	if _, err := txn.Do(ctx, req); err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteByUpsert failed: %v", err)
		return err
	}
	return nil
}
