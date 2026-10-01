package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// CreateOrUpdateBoard upserts a Board node (create when Uid is empty, update
// when Uid is "uid(board)" with the provided upsert query). Mirrors
// CreateOrUpdateDoc.
func CreateOrUpdateBoard(ctx context.Context, query string, dgraphBoard *dgraphStruct.DgraphBoard) (boardUID string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(dgraphBoard)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateBoard failed to marshal dgraphBoard struct err: %+v", err)
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
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateBoard failed to create or update dgraph board err: %+v", err)
		return
	}

	boardUID = res.Uids["uid(board)"]
	defer func() {
		derr := txn.Discard(ctx)
		if derr != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateBoard failed to discard dgraph txn err: %+v", derr)
		}
	}()

	return
}

// SetBoardStateRefAndPurgeInline points the board at its object-storage state
// (board_state_key) and, in the same transaction, deletes any legacy inline
// board_state blob so large boards keep their canvas data out of dgraph. It is
// the persistence path used by the collaboration service.
func SetBoardStateRefAndPurgeInline(ctx context.Context, boardUUID, stateKey, snippet string) (err error) {
	txn := dgraphInit.DgraphClient.NewTxn()
	defer txn.Discard(ctx)

	query := fmt.Sprintf(`query { board as var(func: eq(board_uuid, "%s")) }`, boardUUID)

	currentTime := time.Now()
	set := dgraphStruct.DgraphBoard{
		Uid:       "uid(board)",
		Uuid:      boardUUID,
		StateKey:  stateKey,
		Snippet:   snippet,
		UpdatedAt: &currentTime,
	}
	pb, merr := json.Marshal(set)
	if merr != nil {
		helpers.LogErrorWithContext(ctx, "models/SetBoardStateRefAndPurgeInline marshal err: %+v", merr)
		return merr
	}

	mu := &api.Mutation{
		SetJson:   pb,
		DelNquads: []byte("uid(board) <board_state> * .\n"),
	}
	req := &api.Request{Query: query, Mutations: []*api.Mutation{mu}, CommitNow: true}

	if _, err = txn.Do(ctx, req); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetBoardStateRefAndPurgeInline failed err: %+v", err)
		return
	}
	return
}

// GetDgraphBoardInfoByUUID runs a board query and returns the first board.
func GetDgraphBoardInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphBoardInfoByUUID failed to get board err: %+v", err)
		return
	}

	type Boards struct {
		BoardInfo []*dgraphStruct.DgraphBoard `json:"boardInfo"`
	}

	var boardsInfo Boards
	err = json.Unmarshal(resp.Json, &boardsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphBoardInfoByUUID failed to unmarshal response json err: %+v", err)
		return
	}

	if len(boardsInfo.BoardInfo) == 0 {
		err = errors.New("failed to get dgraph board")
		helpers.LogErrorWithContext(ctx, "models/GetDgraphBoardInfoByUUID failed to get dgraph board")
		return
	}
	dgraphBoard = boardsInfo.BoardInfo[0]

	return
}

// UpdateBoardPermissions adds/removes editors/viewers/commenters on a board in
// a single upsert, mirroring UpdateDocPermissions. Users are resolved by UUID.
func UpdateBoardPermissions(ctx context.Context, input *adapter.InputUpdateBoardPermissions) (err error) {
	txn := dgraphInit.DgraphClient.NewTxn()
	defer txn.Discard(ctx)

	query := fmt.Sprintf(`
		query {
			board as var(func: eq(board_uuid, "%s"))
	`, input.BoardId)

	userVarMap := make(map[string]string)
	varIdx := 0
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

	setRDF := ""
	delRDF := ""
	buildRDF := func(uuids []string, predicate string, isDelete bool) {
		for _, uuid := range uuids {
			if varName, ok := userVarMap[uuid]; ok {
				triple := fmt.Sprintf("uid(board) <%s> uid(%s) .\n", predicate, varName)
				if isDelete {
					delRDF += triple
				} else {
					setRDF += triple
				}
			}
		}
	}
	buildRDF(input.AddEditors, "board_editing_users", false)
	buildRDF(input.RemoveEditors, "board_editing_users", true)
	buildRDF(input.AddViewers, "board_reading_users", false)
	buildRDF(input.RemoveViewers, "board_reading_users", true)
	buildRDF(input.AddCommenters, "board_commenting_users", false)
	buildRDF(input.RemoveCommenters, "board_commenting_users", true)

	if len(setRDF) == 0 && len(delRDF) == 0 {
		return // nothing to do
	}

	mu := &api.Mutation{}
	if len(setRDF) > 0 {
		mu.SetNquads = []byte(setRDF)
	}
	if len(delRDF) > 0 {
		mu.DelNquads = []byte(delRDF)
	}

	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateBoardPermissions failed to update permissions err: %+v", err)
		return
	}
	return
}

// GetDgraphBoardsWithCount runs a board-list query that returns a "boardList"
// block plus a "board_count" aggregation block, mirroring GetDgraphDocsWithCount.
func GetDgraphBoardsWithCount(ctx context.Context, query string, variables map[string]string) (list *dgraphStruct.DgraphBoardList, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphBoardsWithCount failed to query err: %+v", err)
		return
	}

	type Boards struct {
		BoardList []*dgraphStruct.DgraphBoard `json:"boardList"`
		Count     []map[string]int            `json:"board_count"`
	}

	var boardsInfo Boards
	if err = json.Unmarshal(resp.Json, &boardsInfo); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphBoardsWithCount failed to unmarshal err: %+v", err)
		return
	}

	list = &dgraphStruct.DgraphBoardList{Boards: boardsInfo.BoardList}
	for _, v := range boardsInfo.Count {
		list.Count = uint64(v["count"])
	}
	return
}
