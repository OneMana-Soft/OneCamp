package models

import (
	"context"
	"encoding/json"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// ErrNotFound is a doc the graph has no node for, as opposed to a graph that
// didn't answer. It is also helpers.ErrNotFound, which
// helpers.WriteReadFailure answers as a 404 and every other failed read as a
// 503.
var ErrNotFound = helpers.NotFound("failed to get dgraph doc")

func CreateOrUpdateDoc(ctx context.Context, query string, dgraphDoc *dgraphStruct.DgraphDoc, delStringJSON string) (docUID string, err error) {
	pb, err := json.Marshal(dgraphDoc)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreatCreateOrUpdateDoceOrUpdatePost failed to marshal dgraphDoc struct err: %+v",
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
	}

	res, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDoc failed to create or update dgraph doc err: %+v",
			err)
		return
	}

	docUID = res.Uids["uid(doc)"]

	return
}

func GetDgraphDocInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	// Only the tasks the reader may see (models/taskread).
	defer taskread.Settle(ctx, &dgraphDoc, &err)
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDocInfoByUUID failed to get post err: %+v",
			err)
		return
	}

	type Docs struct {
		DocInfo []*dgraphStruct.DgraphDoc `json:"docInfo"`
	}

	var docsInfo Docs
	err = json.Unmarshal(resp.Json, &docsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDocInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	// Nothing by that id is an answer, not a failure: the doc never was, or
	// went for good a moment ago. It isn't logged here; its caller says
	// whether it was expected (helpers.LogReadFailure, WriteReadFailure).
	if len(docsInfo.DocInfo) == 0 {
		err = ErrNotFound
		return
	}
	dgraphDoc = docsInfo.DocInfo[0]

	return
}

func GetDgraphDocs(ctx context.Context, query string, variables map[string]string) (dgraphDocs []*dgraphStruct.DgraphDoc, err error) {
	// Only the tasks the reader may see (models/taskread).
	defer taskread.Settle(ctx, &dgraphDocs, &err)
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDocs failed to get doc err: %+v",
			err)
		return
	}

	type Docs struct {
		DocInfo []*dgraphStruct.DgraphDoc `json:"docInfo"`
	}

	var docsInfo Docs
	err = json.Unmarshal(resp.Json, &docsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDocs failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(docsInfo.DocInfo) == 0 {
		return
	}
	dgraphDocs = docsInfo.DocInfo

	return
}

func GetDgraphDocsWithCount(ctx context.Context, query string, variables map[string]string) (dgraphDocList *dgraphStruct.DgraphDocList, err error) {
	// Only the tasks the reader may see (models/taskread).
	defer taskread.Settle(ctx, &dgraphDocList, &err)
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDocsWithCount failed to get doc err: %+v",
			err)
		return
	}

	type Docs struct {
		DocInfo []*dgraphStruct.DgraphDoc `json:"docInfo"`
		Count   []map[string]int          `json:"doc_count"`
	}

	var docsInfo Docs
	err = json.Unmarshal(resp.Json, &docsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDocsWithCount failed to unmarshal response json err: %+v",
			err)
		return
	}

	dgraphDocList = &dgraphStruct.DgraphDocList{
		Docs: docsInfo.DocInfo,
	}

	if len(docsInfo.Count) > 0 {
		for _, v := range docsInfo.Count {
			dgraphDocList.Count = uint64(v["count"])
		}

	}

	return
}

// UpdateDocPermissions adds/removes a doc's editors/viewers/commenters in a
// single upsert (dgraphStruct.ChangeRoles). Users are resolved by UUID.
func UpdateDocPermissions(ctx context.Context, input *adapter.InputUpdateDocPermissions) error {
	err := dgraphStruct.ChangeRoles(ctx, "doc_uuid", input.DocId, []dgraphStruct.RoleChange{
		{Predicate: "doc_editing_users", Add: input.AddEditors, Remove: input.RemoveEditors},
		{Predicate: "doc_reading_users", Add: input.AddViewers, Remove: input.RemoveViewers},
		{Predicate: "doc_commenting_users", Add: input.AddCommenters, Remove: input.RemoveCommenters},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateDocPermissions failed to update permissions err: %+v", err)
	}
	return err
}
