package domain

import (
	"context"
	"fmt"
	"strconv"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Board"
	commentModels "github.com/akashc777/OneCamp/models/dgraph/Comment"
)

// CreateOrUpdateDgraphBoard upserts a board. For updates, Uid must be
// "uid(board)" and Uuid set so the upsert query resolves the node.
func CreateOrUpdateDgraphBoard(ctx context.Context, dgraphBoard *dgraphStruct.DgraphBoard) (boardUID string, err error) {
	var query string
	if dgraphBoard.Uid == "uid(board)" {
		query = fmt.Sprintf(`query {
									  board as var(func: eq(board_uuid, "%s"))
								  }`, dgraphBoard.Uuid)
	}

	boardUID, err = dgraphModels.CreateOrUpdateBoard(ctx, query, dgraphBoard)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphBoard Failed to create/update board err: %+v", err)
		return
	}
	return
}

// GetDgraphBoardByUUID returns the board with the caller's computed access
// (read/edit/comment) for opening the board. Mirrors GetDgraphDocByUUID.
func GetDgraphBoardByUUID(ctx context.Context, boardUUID string, userUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	variables := make(map[string]string)
	variables["$id"] = boardUUID
	variables["$userId"] = userUID

	query := `query BoardInfo($id: string, $userId: string){
				boardInfo(func: eq(board_uuid, $id)) {
					uid
					board_uuid
					board_title
					board_read_access: count(board_reading_users @filter(uid($userId)))
					board_edit_access: count(board_editing_users @filter(uid($userId)))
					board_comment_access: count(board_commenting_users @filter(uid($userId)))
					board_private
					board_created_at
					board_updated_at
					board_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
				}
			}`

	dgraphBoard, err = dgraphModels.GetDgraphBoardInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphBoardByUUID Failed to get board from dgraph err: %+v", err)
		return
	}
	return
}

// GetBasicDgraphBoardByUUID returns minimal access/ownership info (for
// delete/permission authorization), mirroring GetBasicDgraphDocByUUID.
func GetBasicDgraphBoardByUUID(ctx context.Context, boardUUID string, userUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	variables := make(map[string]string)
	variables["$id"] = boardUUID
	variables["$userId"] = userUID

	query := `query BoardInfo($id: string, $userId: string){
				boardInfo(func: eq(board_uuid, $id)) {
					uid
					board_uuid
					board_read_access: count(board_reading_users @filter(uid($userId)))
					board_edit_access: count(board_editing_users @filter(uid($userId)))
					board_comment_access: count(board_commenting_users @filter(uid($userId)))
					board_private
					board_title
					board_created_by {
						uid
						user_uuid
					}
				}
			}`

	dgraphBoard, err = dgraphModels.GetDgraphBoardInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphBoardByUUID Failed to get board from dgraph err: %+v", err)
		return
	}
	return
}

// GetSystemBoardByUUID returns the board state for the collaboration service
// (no user context; internal-service authenticated upstream).
func GetSystemBoardByUUID(ctx context.Context, boardUUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	variables := make(map[string]string)
	variables["$id"] = boardUUID

	query := `query BoardInfo($id: string){
				boardInfo(func: eq(board_uuid, $id)) {
					uid
					board_uuid
					board_title
					board_state
					board_state_key
					board_snippet
					board_private
					board_created_at
					board_updated_at
				}
			}`

	dgraphBoard, err = dgraphModels.GetDgraphBoardInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetSystemBoardByUUID Failed to get board from dgraph err: %+v", err)
		return
	}
	return
}

// SetBoardStateRef persists the object-storage pointer for a board's canvas
// state and removes any legacy inline blob, in a single upsert.
func SetBoardStateRef(ctx context.Context, boardUUID, stateKey, snippet string) error {
	return dgraphModels.SetBoardStateRefAndPurgeInline(ctx, boardUUID, stateKey, snippet)
}

// UpdateBoardPermissions delegates to the dgraph model upsert.
func UpdateBoardPermissions(ctx context.Context, input *adapter.InputUpdateBoardPermissions) (err error) {
	return dgraphModels.UpdateBoardPermissions(ctx, input)
}

// GetBoardPermissions returns the board with its editing/reading/commenting
// users and owner, for the sharing UI.
func GetBoardPermissions(ctx context.Context, boardUUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	variables := make(map[string]string)
	variables["$id"] = boardUUID

	query := `query BoardPermissions($id: string){
				boardInfo(func: eq(board_uuid, $id)) {
					uid
					board_uuid
					board_private
					board_editing_users {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					board_reading_users {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					board_commenting_users {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					board_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
				}
			}`

	dgraphBoard, err = dgraphModels.GetDgraphBoardInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBoardPermissions Failed to get board from dgraph err: %+v", err)
		return
	}
	return
}

// GetDgraphBoardList returns the boards a user can access (owned or shared via
// edit/read/comment), newest-edited first, with an optional title search and
// pagination. Relies on the @reverse indexes on board_created_by and the
// board_*_users predicates.
func GetDgraphBoardList(ctx context.Context, userUID string, search string, first int, offset int) (list *dgraphStruct.DgraphBoardList, err error) {
	variables := make(map[string]string)
	variables["$userId"] = userUID
	variables["$first"] = strconv.Itoa(first)
	variables["$offset"] = strconv.Itoa(offset)

	notDeleted := `not gt(board_deleted_at, "1970-01-01T00:00:00Z")`
	filter := notDeleted
	if search != "" {
		filter = fmt.Sprintf(`%s AND regexp(board_title, /%s/i)`, notDeleted, search)
	}

	query := fmt.Sprintf(`query BoardList($userId: string, $first: int, $offset: int){
				var(func: uid($userId)) {
					owned as ~board_created_by
					edits as ~board_editing_users
					reads as ~board_reading_users
					comments as ~board_commenting_users
				}

				board_count(func: uid(owned, edits, reads, comments)) @filter(%s) {
					count(uid)
				}

				boardList(func: uid(owned, edits, reads, comments), orderdesc: board_updated_at, first: $first, offset: $offset) @filter(%s) {
					uid
					board_uuid
					board_title
					board_private
					board_thumbnail_key
					board_updated_at
					board_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
				}
			}`, filter, filter)

	list, err = dgraphModels.GetDgraphBoardsWithCount(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphBoardList Failed to get board list err: %+v", err)
		return
	}
	return
}

// CreateBoardCommentInDgraph persists a board-associated Comment (+ inline
// Mention) via an upsert that resolves the board and the (new) comment node.
func CreateBoardCommentInDgraph(ctx context.Context, dgraphComment *dgraphStruct.DgraphComment, boardUUID string, commentUUID string) (commentUID string, err error) {
	query := fmt.Sprintf(`query {
								board as var(func: eq(board_uuid, "%s"))
								co as var(func: eq(comment_uuid, "%s"))
							}`, boardUUID, commentUUID)

	commentUID, err = commentModels.CreateBoardComment(ctx, query, dgraphComment)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateBoardCommentInDgraph failed err: %+v", err)
		return
	}
	return
}

// SoftDeleteBoardCommentInDgraph stamps comment_deleted_at on a board comment
// (resolved by its comment_uuid) so it drops out of the activity feed and any
// reverse-edge reads. Idempotent: a no-op when the comment does not exist.
func SoftDeleteBoardCommentInDgraph(ctx context.Context, commentUUID string, deletedAt time.Time) (err error) {
	query := fmt.Sprintf(`query {
								co as var(func: eq(comment_uuid, "%s"))
							}`, commentUUID)

	dgraphComment := &dgraphStruct.DgraphComment{
		DType:     []string{"Comment"},
		Uid:       "uid(co)",
		Uuid:      commentUUID,
		DeletedAt: &deletedAt,
	}

	if _, err = commentModels.CreateBoardComment(ctx, query, dgraphComment); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SoftDeleteBoardCommentInDgraph failed err: %+v", err)
		return
	}
	return
}
