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

func CreateOrUpdateDgraphProject(ctx context.Context, dgraphProject *dgraphStruct.DgraphProject, query string) (projectUid string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(dgraphProject)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphProject failed to marshal dgraphProject struct err: %+v",
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
			"models/CreateOrUpdateDgraphProject failed to create or update dgraph project err: %+v",
			err)
		return
	}

	projectUid = res.Uids["uid(project)"]

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateDgraphProject failed to discard dgraph txn err: %+v",
				err)
		}
	}()
	return
}

func GetDgraphProjectInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphProjectInfoByUUID failed to get project err: %+v",
			err)
		return
	}

	type Projects struct {
		ProjectInfo []dgraphStruct.DgraphProject `json:"projectInfo"`
	}

	var projectsInfo Projects
	err = json.Unmarshal(resp.Json, &projectsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphProjectInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(projectsInfo.ProjectInfo) == 0 {
		err = errors.New("failed to get dgraph project")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphProjectInfoByUUID failed to get dgraph project variables: %+v", variables)

		return
	}
	dgraphProject = &projectsInfo.ProjectInfo[0]

	return
}

func GetDgraphProjectList(ctx context.Context, query string, variables map[string]string) (dgraphProjects []*dgraphStruct.DgraphProject, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphProjectList failed to get project err: %+v",
			err)
		return
	}

	type Projects struct {
		ProjectInfo []*dgraphStruct.DgraphProject `json:"projectInfo"`
	}

	var projectsInfo Projects
	err = json.Unmarshal(resp.Json, &projectsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphProjectList failed to unmarshal response json err: %+v",
			err)
		return
	}
	dgraphProjects = projectsInfo.ProjectInfo
	return
}

func DeleteProjectEdge(ctx context.Context, delStringJSON string) (err error) {
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
				"models/DeleteProjectEdge failed to discard dgraph txn err: %+v",
				err)
		}

	}()

	return nil
}
