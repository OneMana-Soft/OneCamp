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

// ErrNotFound is a project read that found nothing: an answer, unlike a query
// that failed. Its text is the one this read always gave, and it is also
// helpers.ErrNotFound, which helpers.WriteReadFailure answers as a 404.
var ErrNotFound = helpers.NotFound("failed to get dgraph project")

func CreateOrUpdateDgraphProject(ctx context.Context, dgraphProject *dgraphStruct.DgraphProject, query string) (projectUid string, err error) {
	// What a reader may do in it is laid over a project read for them (Work
	// Access), never written: a project read and written back must not carry
	// it in as a node.
	if dgraphProject != nil && dgraphProject.Access != nil {
		stored := *dgraphProject
		stored.Access = nil
		dgraphProject = &stored
	}
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
	}

	res, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphProject failed to create or update dgraph project err: %+v",
			err)
		return
	}

	projectUid = res.Uids["uid(project)"]

	return
}

func GetDgraphProjectInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()
	return GetDgraphProjectInfoInTxn(ctx, txn, query, variables)
}

// GetDgraphProjectInfoInTxn is GetDgraphProjectInfoByUUID in txn, for a read
// made of several queries that must see one snapshot (dgraphInit.InSnapshot).
func GetDgraphProjectInfoInTxn(ctx context.Context, txn *dgo.Txn, query string, variables map[string]string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
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
		err = ErrNotFound
		// Found nothing: an answer its caller handles (a 404, or skipping
		// the thing), so said at DEBUG, not as a failure.
		helpers.LogDebugWithContext(ctx,
			"models/GetDgraphProjectInfoByUUID failed to get dgraph project variables: %+v", variables)

		return
	}
	dgraphProject = &projectsInfo.ProjectInfo[0]
	// Only the tasks the reader may see (models/taskread).
	if err = taskread.KeepIn(ctx, dgraphProject); err != nil {
		return nil, err
	}
	return
}

func GetDgraphProjectList(ctx context.Context, query string, variables map[string]string) (dgraphProjects []*dgraphStruct.DgraphProject, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

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
	if err = taskread.KeepIn(ctx, &dgraphProjects); err != nil {
		return nil, err
	}
	return
}

func DeleteProjectEdge(ctx context.Context, delStringJSON string) (err error) {
	_, err = dgraphInit.DoCommitNow(ctx, &api.Request{
		Mutations: []*api.Mutation{{DeleteJson: []byte(delStringJSON)}},
	})
	return err
}
