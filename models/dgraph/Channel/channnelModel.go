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

// ErrNotFound is a channel the graph has no node for, as opposed to a graph
// that didn't answer.
var ErrNotFound = errors.New("channel not found in dgraph")

func CreateOrUpdateDgraphChannel(ctx context.Context, dgraphChannel *dgraphStruct.DgraphChannel, query string) (channelUid string, err error) {
	pb, err := json.Marshal(dgraphChannel)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"dgraphModel/CreateOrUpdateDgraphChannel failed to marshal dgraphUser struct err: %+v",
			err)
		return
	}

	// An upsert on the channel node, so two writes to one channel at once (two
	// people joining it, say) conflict, and Dgraph aborts one and asks for a
	// retry. DoCommitNow retries it rather than failing the loser.
	res, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{SetJson: pb}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"dgraphModel/CreateOrUpdateDgraphChannel dgraph txn failed err: %+v",
			err)
		return
	}
	// uid will only get assigned only if new object gets created
	channelUid = res.Uids["uid(ch)"]
	return
}

func GetDgraphChannelsInfoByUserUUID(ctx context.Context, query string, variables map[string]string) (dgraphChannel []*dgraphStruct.DgraphChannel, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelsInfoByUserUUID failed to get user err: %+v",
			err)
		return
	}

	type Channels struct {
		ChannelInfo []dgraphStruct.DgraphChannel `json:"channelInfo"`
	}

	var channelInfo Channels
	err = json.Unmarshal(resp.Json, &channelInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelsInfoByUserUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(channelInfo.ChannelInfo) == 0 {

		return
	}

	for i := 0; i < len(channelInfo.ChannelInfo); i++ {
		dgraphChannel = append(dgraphChannel, &channelInfo.ChannelInfo[i])
	}

	return
}

func GetDgraphChannelsInfoAndCount(ctx context.Context, query string, variables map[string]string) (dgraphChannel []*dgraphStruct.DgraphChannel, totalCount int64, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelsInfoAndCount failed to get channels err: %+v",
			err)
		return
	}

	type CountStruct struct {
		Count int64 `json:"count"`
	}

	type Channels struct {
		ChannelInfo []dgraphStruct.DgraphChannel `json:"channelInfo"`
		TotalCount  []CountStruct                `json:"totalCount"`
	}

	var channelInfo Channels
	err = json.Unmarshal(resp.Json, &channelInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelsInfoAndCount failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(channelInfo.TotalCount) > 0 {
		totalCount = channelInfo.TotalCount[0].Count
	}

	if len(channelInfo.ChannelInfo) == 0 {
		return
	}

	for i := 0; i < len(channelInfo.ChannelInfo); i++ {
		dgraphChannel = append(dgraphChannel, &channelInfo.ChannelInfo[i])
	}

	return
}

func GetDgraphChannelInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphChannel *dgraphStruct.DgraphChannel, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelInfoByUUID failed to get user err: %+v",
			err)
		return
	}

	type Channels struct {
		ChannelInfo []dgraphStruct.DgraphChannel `json:"channelInfo"`
	}

	var channelInfo Channels
	err = json.Unmarshal(resp.Json, &channelInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(channelInfo.ChannelInfo) == 0 {
		err = ErrNotFound
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelInfoByUUID failed to get channelInfo")
		return
	}
	dgraphChannel = &channelInfo.ChannelInfo[0]

	return
}

func DeleteChannelEdge(ctx context.Context, delStringJSON string) (err error) {
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

func GetDgraphChannelInfoByName(ctx context.Context, query string, variables map[string]string) (dgraphChannel *dgraphStruct.DgraphChannel, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelInfoByName failed to get user err: %+v",
			err)
		return
	}

	type Channels struct {
		ChannelInfo []dgraphStruct.DgraphChannel `json:"channelInfo"`
	}

	var channelInfo Channels
	err = json.Unmarshal(resp.Json, &channelInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelInfoByName failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(channelInfo.ChannelInfo) == 0 {
		err = ErrNotFound
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphChannelInfoByName failed to get channel info")
		return
	}
	dgraphChannel = &channelInfo.ChannelInfo[0]

	return
}
