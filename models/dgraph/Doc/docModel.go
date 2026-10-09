package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// ErrSharingIDs refuses a sharing change naming something that isn't a uuid:
// the ids are written into the change's query.
var ErrSharingIDs = errors.New("a doc and the people it's shared with are named by their uuids")

// sharingIDsValid reports whether the doc and every person a sharing change
// names is a uuid, written as one is.
func sharingIDsValid(input *adapter.InputUpdateDocPermissions) bool {
	valid := func(s string) bool {
		id, err := uuid.Parse(s)
		return err == nil && id.String() == s
	}
	if !valid(input.DocId) {
		return false
	}
	for _, list := range [][]string{input.AddEditors, input.RemoveEditors, input.AddViewers, input.RemoveViewers, input.AddCommenters, input.RemoveCommenters} {
		for _, s := range list {
			if s != "" && !valid(s) {
				return false
			}
		}
	}
	return true
}

// ErrNotFound is a doc the graph has no node for, as opposed to a graph that
// didn't answer.
var ErrNotFound = errors.New("failed to get dgraph doc")

func CreateOrUpdateDoc(ctx context.Context, query string, dgraphDoc *dgraphStruct.DgraphDoc, delStringJSON string) (docUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDoc failed to create or update dgraph doc err: %+v",
			err)
		return
	}

	docUID = res.Uids["uid(doc)"]
	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateDoc failed to discard dgraph txn err: %+v",
				err)
		}
	}()

	return
}

func GetDgraphDocInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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

	if len(docsInfo.DocInfo) == 0 {
		err = ErrNotFound
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphDocInfoByUUID failed to get dgraph doc")

		return
	}
	dgraphDoc = docsInfo.DocInfo[0]

	return
}

func GetDgraphDocs(ctx context.Context, query string, variables map[string]string) (dgraphDocs []*dgraphStruct.DgraphDoc, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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
	txn := dgraphInit.DgraphClient.NewTxn()

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

func UpdateDocPermissions(ctx context.Context, input *adapter.InputUpdateDocPermissions) (err error) {
	// Every id below is written into the query as text, so each must be a uuid.
	if !sharingIDsValid(input) {
		return ErrSharingIDs
	}
	txn := dgraphInit.DgraphClient.NewTxn()
	defer txn.Discard(ctx)

	// 1. Build Query Block
	// We need variables for the Doc and each User involved.
	query := fmt.Sprintf(`
		query {
			doc as var(func: eq(doc_uuid, "%s"))
	`, input.DocId)

	// Map generic user UUIDs to variable names for the query
	// uuid -> varName
	userVarMap := make(map[string]string)
	varIdx := 0

	// Helper to add user vars to query
	addVars := func(uuids []string) {
		for _, uuid := range uuids {
			if _, exists := userVarMap[uuid]; !exists && uuid != "" {
				varName := fmt.Sprintf("u_%d", varIdx)
				query += fmt.Sprintf(`%s as var(func: eq(user_uuid, "%s"))
				`, varName, uuid)
				userVarMap[uuid] = varName
				varIdx++
			}
		}
	}

	addVars(input.AddEditors)
	addVars(input.RemoveEditors)
	addVars(input.AddViewers)
	addVars(input.RemoveViewers)
	addVars(input.AddCommenters)
	addVars(input.RemoveCommenters)

	query += "}"

	// 2. Build Mutation RDFs
	setRDF := ""
	delRDF := ""

	buildRDF := func(uuids []string, predicate string, isDelete bool) {
		for _, uuid := range uuids {
			if varName, ok := userVarMap[uuid]; ok {
				triple := fmt.Sprintf("uid(doc) <%s> uid(%s) .\n", predicate, varName)
				if isDelete {
					delRDF += triple
				} else {
					setRDF += triple
				}
			}
		}
	}

	buildRDF(input.AddEditors, "doc_editing_users", false)
	buildRDF(input.RemoveEditors, "doc_editing_users", true)

	buildRDF(input.AddViewers, "doc_reading_users", false)
	buildRDF(input.RemoveViewers, "doc_reading_users", true)

	buildRDF(input.AddCommenters, "doc_commenting_users", false)
	buildRDF(input.RemoveCommenters, "doc_commenting_users", true)

	mu := &api.Mutation{}
	if len(setRDF) > 0 {
		mu.SetNquads = []byte(setRDF)
	}
	if len(delRDF) > 0 {
		mu.DelNquads = []byte(delRDF)
	}

	if len(setRDF) == 0 && len(delRDF) == 0 {
		return // Nothing to do
	}

	// helpers.MessageLogs.InfoLog.Printf("models/UpdateDocPermissions Query: %s", query)
	// helpers.MessageLogs.InfoLog.Printf("models/UpdateDocPermissions SetRDF: %s", setRDF)
	// helpers.MessageLogs.InfoLog.Printf("models/UpdateDocPermissions DelRDF: %s", delRDF)

	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateDocPermissions failed to update permissions err: %+v",
			err)
		return
	}

	return
}

// BulkSoftDeleteDgraphDocs sets doc_deleted_at on multiple docs in a single Dgraph mutation.
func BulkSoftDeleteDgraphDocs(ctx context.Context, docs []*dgraphStruct.DgraphDoc, query string) error {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(docs)
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
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphDocs failed: %v", err)
		return err
	}

	return nil
}
