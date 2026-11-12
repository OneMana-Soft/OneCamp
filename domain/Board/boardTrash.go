package domain

// Deleted boards (UX-TB-H2-BE, TABLES-BOARDS-13), as the graph answers them:
// a page of the boards in the trash a person may bring back, and bringing one
// back. business/Board/boardTrash.go decides who may.

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// liveDeletedAt is the deletion time a live board carries, as CreateBoard
// writes it (0001-01-01: never deleted).
const liveDeletedAt = "0001-01-01T00:00:00Z"

// inTrash is the filter of a board in the trash: deleted, and not scratch (a
// board the app made and took away again, never anyone's to bring back).
func inTrash() string {
	return dgraphquery.Deleted("board_deleted_at") + " AND NOT eq(board_scratch, true)"
}

// GetBoardTrash reads a page of the boards in the trash a person (graph uid)
// may bring back, newest deletion first: the boards they made, and with
// others, every board in the trash that isn't private too (a workspace
// admin's list).
func GetBoardTrash(ctx context.Context, userUID string, others bool, first, offset int) ([]*dgraphStruct.DgraphBoard, error) {
	if !helpers.IsDgraphUID(userUID) {
		return nil, nil
	}
	deleted := inTrash()
	from, open := "uid(owned)", ""
	if others {
		from = "uid(owned, open)"
		open = `open as var(func: type(Board)) @filter(` + deleted + ` AND NOT eq(board_private, true))`
	}
	boards, err := dgexec.List[*dgraphStruct.DgraphBoard](ctx, `query Trash($userId: string) {
			var(func: uid($userId)) {
				owned as ~board_created_by
			}
			`+open+`
			boards(func: `+from+`, orderdesc: board_deleted_at, first: `+strconv.Itoa(first)+`, offset: `+strconv.Itoa(offset)+`) @filter(type(Board) AND `+deleted+`) {
				board_uuid
				board_title
				board_private
				board_deleted_at
				board_scratch
				board_created_by { user_uuid user_name user_full_name user_profile_object_key }
			}
		}`, map[string]string{"$userId": userUID}, "boards")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetBoardTrash err: %+v", err)
	}
	return boards, err
}

// RestoreTrashedBoard takes a board out of the trash: its deletion time back
// to a live board's, in one conditional upsert that writes only while the
// board is in the graph and in the trash (a scratch board never is). A board
// a retention rule removed is gone from the graph (PurgeBoardNodes), and an
// upsert keyed by its uuid alone would write it back as a new, bare, live
// node; and a restore that meets the rule's delete in flight either finds
// the board gone and writes nothing, or lands first, when the delete, which
// asks again which boards are still in the trash, leaves it. Answers whether
// it brought the board back.
func RestoreTrashedBoard(ctx context.Context, boardUUID string) (bool, error) {
	if _, err := uuid.Parse(boardUUID); err != nil {
		return false, nil
	}
	res, err := dgexec.Mutate(ctx, `query Restore($id: string) {
			board as var(func: eq(board_uuid, $id)) @filter(type(Board) AND `+inTrash()+`)
			restored(func: uid(board)) { uid }
		}`, map[string]string{"$id": boardUUID}, dgexec.Mutation{
		SetNQuads: `uid(board) <board_deleted_at> "` + liveDeletedAt + `" .`,
		Cond:      "@if(eq(len(board), 1))",
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RestoreTrashedBoard %s err: %+v", boardUUID, err)
		return false, err
	}
	var found struct {
		Restored []struct {
			UID string `json:"uid"`
		} `json:"restored"`
	}
	if err := json.Unmarshal(res.GetJson(), &found); err != nil {
		return false, err
	}
	return len(found.Restored) == 1, nil
}
