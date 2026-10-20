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

// ErrNotFound is a post read that found nothing: an answer, unlike a query
// that failed. Its text is the one this read always gave.
var ErrNotFound = errors.New("failed to get dgraph user")

// CreateOrUpdatePost runs the post's upsert. Posts written into one channel
// at once each add themselves to its ch_posts, and Dgraph aborts all but one
// of the overlapping transactions; dgraphInit.DoCommitNow runs an aborted one
// again, so every post lands (tests/integration/messages_at_once_test.go: until
// 19 Oct 2026 one of two lines or posts sent together was lost).
func CreateOrUpdatePost(ctx context.Context, query string, dgraphPost *dgraphStruct.DgraphPost, delStringJSON string) (postUID string, err error) {
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

	res, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdatePost failed to create or update dgraph post err: %+v",
			err)
		return
	}
	postUID = res.Uids["uid(po)"]
	return
}

func CreateOrUpdatePostReaction(ctx context.Context, query string, dgraphPost *dgraphStruct.DgraphPost) (reactionUID string, err error) {
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
	}

	res, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdatePostReactiont failed to create or update dgraph post reaction err: %+v",
			err)
		return
	}

	for _, v := range res.Uids {
		reactionUID = v
	}
	return
}

func GetDgraphPostInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphUser *dgraphStruct.DgraphPost, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()

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
		err = ErrNotFound
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostInfoByUUID failed to get dgraph post")

		return
	}
	dgraphUser = &postsInfo.PostInfo[0]
	// A forwarded original deleted since shows as deleted, never its text.
	dgraphUser.HideDeletedForwards()

	return
}

func GetDgraphPosts(ctx context.Context, query string, variables map[string]string) (dgraphPost []*dgraphStruct.DgraphPost, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()

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
	// A forwarded original deleted since shows as deleted, never its text.
	for _, p := range dgraphPost {
		p.HideDeletedForwards()
	}

	return
}

func DeleteNodeOrEdges(ctx context.Context, delStringJSON string) (err error) {
	_, err = dgraphInit.DoCommitNow(ctx, &api.Request{
		Mutations: []*api.Mutation{{DeleteJson: []byte(delStringJSON)}},
	})
	return err
}

// BulkSoftDeleteDgraphPosts sets post_deleted_at on multiple posts in a single Dgraph mutation.
func BulkSoftDeleteDgraphPosts(ctx context.Context, posts []*dgraphStruct.DgraphPost, query string) error {
	pb, err := json.Marshal(posts)
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
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphPosts failed: %v", err)
		return err
	}
	return nil
}

// DeleteByUpsert deletes the nodes a query names: the delete JSON refers to
// the query's variables ("uid(a)"), so the lookup and the delete happen in one
// request. The sibling of DeleteNodeOrEdges for callers that hold ids, not uids.
func DeleteByUpsert(ctx context.Context, query string, delJSON string) error {
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{DeleteJson: []byte(delJSON)}},
	}
	if _, err := dgraphInit.DoCommitNow(ctx, req); err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteByUpsert failed: %v", err)
		return err
	}
	return nil
}
