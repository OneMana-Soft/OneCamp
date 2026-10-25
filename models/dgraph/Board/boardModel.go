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
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// CreateOrUpdateBoard upserts a Board node (create when Uid is empty, update
// when Uid is "uid(board)" with the provided upsert query). Mirrors
// CreateOrUpdateDoc.
func CreateOrUpdateBoard(ctx context.Context, query string, dgraphBoard *dgraphStruct.DgraphBoard) (boardUID string, err error) {
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
	}

	res, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateBoard failed to create or update dgraph board err: %+v", err)
		return
	}

	boardUID = res.Uids["uid(board)"]

	return
}

// SetBoardStateRefAndPurgeInline points the board at its object-storage state
// (board_state_key) and, in the same transaction, deletes any legacy inline
// board_state blob so large boards keep their canvas data out of dgraph. It is
// the persistence path used by the collaboration service.
func SetBoardStateRefAndPurgeInline(ctx context.Context, boardUUID, stateKey, snippet string) (err error) {
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
	req := &api.Request{Query: query, Mutations: []*api.Mutation{mu}}

	if _, err = dgraphInit.DoCommitNow(ctx, req); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetBoardStateRefAndPurgeInline failed err: %+v", err)
		return
	}
	return
}

// GetDgraphBoardInfoByUUID runs a board query and returns the first board.
func GetDgraphBoardInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	// Only the tasks the reader may see (models/taskread).
	defer taskread.Settle(ctx, &dgraphBoard, &err)
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

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

// GetDgraphBoards runs a board query and returns every board in its
// "boardInfo" block. Finding none is not an error.
func GetDgraphBoards(ctx context.Context, query string, variables map[string]string) ([]*dgraphStruct.DgraphBoard, error) {
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphBoards failed to get boards err: %+v", err)
		return nil, err
	}
	var boardsInfo struct {
		BoardInfo []*dgraphStruct.DgraphBoard `json:"boardInfo"`
	}
	if err := json.Unmarshal(resp.Json, &boardsInfo); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphBoards failed to unmarshal response json err: %+v", err)
		return nil, err
	}
	// Only the tasks the reader may see (models/taskread).
	if err := taskread.KeepIn(ctx, &boardsInfo.BoardInfo); err != nil {
		return nil, err
	}
	return boardsInfo.BoardInfo, nil
}

// UpdateBoardPermissions adds/removes editors/viewers/commenters on a board in
// a single upsert (dgraphStruct.ChangeRoles), as a doc's sharing does. Users
// are resolved by UUID.
func UpdateBoardPermissions(ctx context.Context, input *adapter.InputUpdateBoardPermissions) error {
	err := dgraphStruct.ChangeRoles(ctx, "board_uuid", input.BoardId, []dgraphStruct.RoleChange{
		{Predicate: "board_editing_users", Add: input.AddEditors, Remove: input.RemoveEditors},
		{Predicate: "board_reading_users", Add: input.AddViewers, Remove: input.RemoveViewers},
		{Predicate: "board_commenting_users", Add: input.AddCommenters, Remove: input.RemoveCommenters},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateBoardPermissions failed to update permissions err: %+v", err)
	}
	return err
}

// GetDgraphBoardsWithCount runs a board-list query that returns a "boardList"
// block plus a "board_count" aggregation block, mirroring GetDgraphDocsWithCount.
func GetDgraphBoardsWithCount(ctx context.Context, query string, variables map[string]string) (list *dgraphStruct.DgraphBoardList, err error) {
	// Only the tasks the reader may see (models/taskread).
	defer taskread.Settle(ctx, &list, &err)
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

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
