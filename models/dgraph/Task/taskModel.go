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

func CreateOrUpdateDgraphTask(ctx context.Context, dgraphTask *dgraphStruct.DgraphTask, query string, delStringJSON string) (taskUid string, err error) {
	pb, err := json.Marshal(dgraphTask)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphTask failed to marshal dgraphTask struct err: %+v",
			err)
		return
	}

	var mutations []*api.Mutation
	if len(delStringJSON) > 0 {
		muDelete := &api.Mutation{
			DeleteJson: []byte(delStringJSON),
		}
		mutations = append(mutations, muDelete)
	}

	muSet := &api.Mutation{
		SetJson: pb,
	}
	mutations = append(mutations, muSet)

	// Run again in a fresh transaction if Dgraph aborts it for a concurrent
	// write to the same task (a card dropped into another row and column
	// saves its status and its assignee at once).
	res, err := dgraphInit.DoCommitNow(ctx, &api.Request{Query: query, Mutations: mutations})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphTask failed to create or update dgraph task err: %+v",
			err)
		return
	}

	// will get uid only when new node is created
	taskUid = res.Uids["uid(task)"]
	return
}

func GetDgraphTaskInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphTask *dgraphStruct.DgraphTask, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTaskInfoByUUID failed to get task err: %+v",
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
			"models/GetDgraphTaskInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(tasksInfo.TaskInfo) == 0 {
		err = errors.New("failed to get dgraph task")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTaskInfoByUUID failed to get dgraph task")

		return
	}
	dgraphTask = &tasksInfo.TaskInfo[0]

	return
}

// BulkSoftDeleteDgraphTasks sets task_deleted_at on multiple tasks in a single Dgraph mutation.
func BulkSoftDeleteDgraphTasks(ctx context.Context, tasks []*dgraphStruct.DgraphTask, query string) error {
	pb, err := json.Marshal(tasks)
	if err != nil {
		return err
	}
	_, err = dgraphInit.DoCommitNow(ctx, &api.Request{Query: query, Mutations: []*api.Mutation{{SetJson: pb}}})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphTasks failed: %v", err)
		return err
	}
	return nil
}

// QueryDgraphTasks runs a read-only query and returns every task found in any
// block of the result, at the top level or one level down (a block over a
// project or user that lists its tasks under "tasks").
func QueryDgraphTasks(ctx context.Context, query string, variables map[string]string) ([]*dgraphStruct.DgraphTask, error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/QueryDgraphTasks failed: %v", err)
		return nil, err
	}
	var blocks map[string][]struct {
		dgraphStruct.DgraphTask
		Tasks []*dgraphStruct.DgraphTask `json:"tasks"`
	}
	if err := json.Unmarshal(resp.Json, &blocks); err != nil {
		return nil, err
	}
	var out []*dgraphStruct.DgraphTask
	for _, rows := range blocks {
		for i := range rows {
			if rows[i].Uuid != "" {
				t := rows[i].DgraphTask
				out = append(out, &t)
			}
			out = append(out, rows[i].Tasks...)
		}
	}
	return out, nil
}

// UpdateExistingTasks applies set (and delJSON, if any) to the tasks in the
// query's var "task", and only when there are some: an unconditional upsert
// on an empty var creates a new node instead of doing nothing.
func UpdateExistingTasks(ctx context.Context, set *dgraphStruct.DgraphTask, query string, delJSON string) error {
	_, err := UpdateExistingTasksReturning(ctx, set, query, delJSON)
	return err
}

// UpdateExistingTasksReturning is UpdateExistingTasks for a query that also
// names the tasks it matches, in a block called affected selecting task_uuid.
// It returns their uuids as they were matched, before the change, in the same
// request as the change, so no task can be missed or counted twice.
func UpdateExistingTasksReturning(ctx context.Context, set *dgraphStruct.DgraphTask, query string, delJSON string) ([]string, error) {
	pb, err := json.Marshal(set)
	if err != nil {
		return nil, err
	}
	const cond = "@if(gt(len(task), 0))"
	var mutations []*api.Mutation
	if delJSON != "" {
		mutations = append(mutations, &api.Mutation{DeleteJson: []byte(delJSON), Cond: cond})
	}
	mutations = append(mutations, &api.Mutation{SetJson: pb, Cond: cond})
	resp, err := dgraphInit.DoCommitNow(ctx, &api.Request{Query: query, Mutations: mutations})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateExistingTasks err: %+v", err)
		return nil, err
	}
	var out struct {
		Affected []struct {
			Uuid string `json:"task_uuid"`
		} `json:"affected"`
	}
	if len(resp.GetJson()) > 0 {
		if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
			return nil, err
		}
	}
	uuids := make([]string, 0, len(out.Affected))
	for _, a := range out.Affected {
		if a.Uuid != "" {
			uuids = append(uuids, a.Uuid)
		}
	}
	return uuids, nil
}

func GetDgraphTaskList(ctx context.Context, query string, variables map[string]string) (dgraphTasks []*dgraphStruct.DgraphTask, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTaskList failed to get task err: %+v",
			err)
		return
	}

	type Tasks struct {
		TaskInfo []*dgraphStruct.DgraphTask `json:"taskInfo"`
	}

	var tasksInfo Tasks
	err = json.Unmarshal(resp.Json, &tasksInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTaskList failed to unmarshal response json err: %+v",
			err)
		return
	}
	dgraphTasks = tasksInfo.TaskInfo
	return
}
