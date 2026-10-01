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

func CreateOrUpdateDgraphTeam(ctx context.Context, dgraphTeam *dgraphStruct.DgraphTeam, query string) (userUid string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(dgraphTeam)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphTeam failed to marshal dgraphTeam struct err: %+v",
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
			"models/CreateOrUpdateDgraphTeam failed to create or update dgraph team err: %+v",
			err)
		return
	}

	// will get uid only when new node is created
	userUid = res.Uids["uid(team)"]

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateDgraphTeam failed to discard dgraph txn err: %+v",
				err)
		}
	}()
	return
}

func GetDgraphTeamInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTeamInfoByUUID failed to get team err: %+v",
			err)
		return
	}

	type Teams struct {
		TeamInfo []dgraphStruct.DgraphTeam `json:"teamInfo"`
	}

	var teamsInfo Teams
	err = json.Unmarshal(resp.Json, &teamsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTeamInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(teamsInfo.TeamInfo) == 0 {
		err = errors.New("failed to get dgraph team")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTeamInfoByUUID failed to get dgraph team variables: %+v", variables)

		return
	}
	dgraphTeam = &teamsInfo.TeamInfo[0]

	return
}

func GetDgraphTeamList(ctx context.Context, query string, variables map[string]string) (dgraphTeams []*dgraphStruct.DgraphTeam, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTeamList failed to get team err: %+v",
			err)
		return
	}

	type Teams struct {
		TeamInfo []*dgraphStruct.DgraphTeam `json:"teamInfo"`
	}

	var teamsInfo Teams
	err = json.Unmarshal(resp.Json, &teamsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTeamList failed to unmarshal response json err: %+v",
			err)
		return
	}
	dgraphTeams = teamsInfo.TeamInfo
	return
}

func DeleteTeamEdge(ctx context.Context, delStringJSON string) (err error) {
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
				"models/DeleteTeamEdge failed to discard dgraph txn err: %+v",
				err)
		}

	}()

	return nil
}
