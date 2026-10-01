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

func CreateOrUpdateCommentInPost(ctx context.Context, query string, dgraphPost *dgraphStruct.DgraphPost) (commentUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphPost)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateCommentInPost failed to marshal dgraphComment struct err: %+v",
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
			"models/CreateCommentInPost failed to create dgraph comment err: %+v",
			err)
		return
	}

	for _, v := range res.Uids {
		commentUID = v
	}
	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateCommentInPost failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func CreateOrUpdateCommentInDoc(ctx context.Context, query string, dgraphDoc *dgraphStruct.DgraphDoc) (commentUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphDoc)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateCommentInDoc failed to marshal dgraphComment struct err: %+v",
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
			"models/CreateOrUpdateCommentInDoc failed to create dgraph comment err: %+v",
			err)
		return
	}

	for _, v := range res.Uids {
		commentUID = v
	}
	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateCommentInDoc failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func CreateOrUpdateCommentInTask(ctx context.Context, query string, dgraphTask *dgraphStruct.DgraphTask) (commentUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphTask)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateCommentInTask failed to marshal dgraphComment struct err: %+v",
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
			"models/CreateOrUpdateCommentInTask failed to create dgraph comment err: %+v",
			err)
		return
	}

	for _, v := range res.Uids {
		commentUID = v
	}
	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateCommentInTask failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func UpdateComment(ctx context.Context, query string, dgraphPost *dgraphStruct.DgraphComment, deleteJson string) (err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphPost)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateComment failed to marshal dgraphComment struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson:    pb,
		DeleteJson: []byte(deleteJson),
	}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateComment failed to update dgraph comment err: %+v",
			err)
		return
	}

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/UpdateComment failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func GetDgraphTaskCommentInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphTask *dgraphStruct.DgraphTask, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTaskCommentInfoByUUID failed to get task err: %+v",
			err)
		return
	}

	type Tasks struct {
		TaskInfo []dgraphStruct.DgraphTask `json:"taskInfo"`
	}

	var tasksInfo Tasks
	err = json.Unmarshal(resp.Json, &tasksInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTaskCommentInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(tasksInfo.TaskInfo) == 0 {
		err = errors.New("failed to get dgraph task")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTaskCommentInfoByUUID failed to get dgraph task")

		return
	}
	dgraphTask = &tasksInfo.TaskInfo[0]

	return
}

func GetDgraphPostCommentsInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphUser *dgraphStruct.DgraphPost, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostCommentsInfoByUUID failed to get post err: %+v",
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
			"models/GetDgraphPostCommentsInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(postsInfo.PostInfo) == 0 {
		err = errors.New("failed to get dgraph user")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostCommentsInfoByUUID failed to get dgraph post")

		return
	}
	dgraphUser = &postsInfo.PostInfo[0]

	return
}

func GetDgraphCommentInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphUser *dgraphStruct.DgraphComment, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphCommentInfoByUUID failed to get post err: %+v",
			err)
		return
	}

	type Comments struct {
		CommentInfo []dgraphStruct.DgraphComment `json:"commentInfo"`
	}

	var commentsInfo Comments
	err = json.Unmarshal(resp.Json, &commentsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(commentsInfo.CommentInfo) == 0 {
		err = errors.New("failed to get dgraph user")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphCommentInfoByUUID failed to get dgraph post")

		return
	}
	dgraphUser = &commentsInfo.CommentInfo[0]

	return
}

func CreateOrUpdateCommentReaction(ctx context.Context, query string, dgraphPost *dgraphStruct.DgraphComment) (reactionUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphPost)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdatePostCommentReaction failed to marshal dgraphPost struct err: %+v",
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
			"models/CreateOrUpdatePostCommentReaction failed to create or update dgraph post reaction err: %+v",
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
				"models/CreateOrUpdatePostCommentReaction failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func DeleteReactionNodeOrEdges(ctx context.Context, delStringJSON string) (err error) {
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
				"models/DeleteReactionNodeOrEdges failed to discard dgraph txn err: %+v",
				err)
		}

	}()

	return nil
}

func CreateOrUpdateCommentInChat(ctx context.Context, query string, dgraphChat *dgraphStruct.DgraphChat) (commentUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphChat)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateCommentInChat failed to marshal dgraphChat struct err: %+v",
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
			"models/CreateOrUpdateCommentInChat failed to create dgraph chat err: %+v",
			err)
		return
	}

	for _, v := range res.Uids {
		commentUID = v
	}
	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateCommentInChat failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

// CreateBoardComment persists a Comment node associated with a board (via
// comment_board) plus an inline Mention node. The top-level mutated object is
// the comment itself (boards have no board_comments edge). The upsert query
// must define `board`, `co`, and any `u_N` (mention user) vars.
func CreateBoardComment(ctx context.Context, query string, dgraphComment *dgraphStruct.DgraphComment) (commentUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphComment)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateBoardComment failed to marshal comment err: %+v", err)
		return
	}

	mu := &api.Mutation{SetJson: pb}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateBoardComment failed to create comment err: %+v", err)
		return
	}

	for _, v := range res.Uids {
		commentUID = v
	}
	defer func() {
		if derr := txn.Discard(ctx); derr != nil {
			helpers.LogErrorWithContext(ctx, "models/CreateBoardComment failed to discard txn err: %+v", derr)
		}
	}()
	return
}
