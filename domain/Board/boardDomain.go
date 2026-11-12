package domain

import (
	"context"
	"fmt"
	"github.com/akashc777/OneCamp/models/dgexec"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
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
					board_deleted_at
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
	// A uuid naming no board is the read's NotFound, an answer: not logged.
	helpers.LogReadFailure(ctx, err, "domain/GetDgraphBoardByUUID failed to get board from dgraph")
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
					board_deleted_at
					board_scratch
					board_created_by {
						uid
						user_uuid
					}
				}
			}`

	dgraphBoard, err = dgraphModels.GetDgraphBoardInfoByUUID(ctx, query, variables)
	// A uuid naming no board is the read's NotFound, an answer: not logged.
	helpers.LogReadFailure(ctx, err, "domain/GetBasicDgraphBoardByUUID failed to get board from dgraph")
	return
}

// GetBoardsReadAccessFor reads, for one person (by uuid), what CanRead decides
// each of the named boards by: its privacy, its deletion time, its creator,
// and whether it is shared with them; and its title, for a caller naming those
// they may read. One query for the lot; ids that aren't uuids are left out,
// and so is any board that doesn't exist.
func GetBoardsReadAccessFor(ctx context.Context, boardUUIDs []string, userUUID string) ([]*dgraphStruct.DgraphBoard, error) {
	list := dgraphquery.UUIDList(boardUUIDs)
	if list == "" {
		return nil, nil
	}
	query := `query Readable($user: string) {
				boardInfo(func: eq(board_uuid, [` + list + `])) {
					board_uuid
					board_title
					board_private
					board_deleted_at
					board_created_by {
						user_uuid
					}
					board_read_access: count(board_reading_users @filter(eq(user_uuid, $user)))
					board_edit_access: count(board_editing_users @filter(eq(user_uuid, $user)))
					board_comment_access: count(board_commenting_users @filter(eq(user_uuid, $user)))
				}
			}`
	boards, err := dgraphModels.GetDgraphBoards(ctx, query, map[string]string{"$user": userUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetBoardsReadAccessFor err: %+v", err)
	}
	return boards, err
}

// GetLiveBoardUUIDPage reads the uuids of one page of every live board (none
// deleted), in the graph's order: for reading every board a page at a time
// (the workspace export).
func GetLiveBoardUUIDPage(ctx context.Context, offset, first int) ([]string, error) {
	ids, err := dgexec.UUIDPage(ctx, "board_uuid", dgraphquery.Live("board_deleted_at"), offset, first)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetLiveBoardUUIDPage err: %+v", err)
	}
	return ids, err
}

// GetBoardsForText reads, in one query, what writing several boards' text
// needs: each one's state (inline, or the key of the object that holds it)
// and its sharing, as GetBoardPermissions reads it. Ids that aren't uuids are
// left out.
func GetBoardsForText(ctx context.Context, boardUUIDs []string) ([]*dgraphStruct.DgraphBoard, error) {
	list := dgraphquery.UUIDList(boardUUIDs)
	if list == "" {
		return nil, nil
	}
	boards, err := dgexec.List[*dgraphStruct.DgraphBoard](ctx, `{
		boards(func: eq(board_uuid, [`+list+`])) {
			board_state
			board_state_key
			`+boardSharingBlock+`
		}
	}`, nil, "boards")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetBoardsForText err: %+v", err)
	}
	return boards, err
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
	// A uuid naming no board is the read's NotFound, an answer: not logged.
	helpers.LogReadFailure(ctx, err, "domain/GetSystemBoardByUUID failed to get board from dgraph")
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

// boardSharingBlock is what GetBoardPermissions reads of a board: its title,
// deletion time and privacy, its owner and the people it is shared with.
const boardSharingBlock = `uid
					board_uuid
					board_title
					board_deleted_at
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
					}`

// GetBoardPermissions returns the board with its editing/reading/commenting
// users and owner, for the sharing UI.
func GetBoardPermissions(ctx context.Context, boardUUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	variables := make(map[string]string)
	variables["$id"] = boardUUID

	query := `query BoardPermissions($id: string){
				boardInfo(func: eq(board_uuid, $id)) {
					` + boardSharingBlock + `
				}
			}`

	dgraphBoard, err = dgraphModels.GetDgraphBoardInfoByUUID(ctx, query, variables)
	// A uuid naming no board is the read's NotFound, an answer: not logged.
	helpers.LogReadFailure(ctx, err, "domain/GetBoardPermissions failed to get board from dgraph")
	return
}

// Whose boards a board list keeps (BoardListFilter.Owner).
const (
	// BoardsMine is the boards the viewer made.
	BoardsMine = "mine"
	// BoardsShared is the boards shared with the viewer, less their own.
	BoardsShared = "shared"
)

// BoardListFilter narrows a board list (miro-46); the zero filter is every
// board the viewer may read. A filter only ever narrows that list: it never
// adds a board the viewer may not read. ProjectUUID and TeamUUID are one or
// the other, and the caller has checked the viewer may read the project or
// team (controllers/Board's GetBoardList).
type BoardListFilter struct {
	// Editable keeps the boards the viewer may change (CanEdit: owned, or
	// shared to edit), as a call's board picker lists.
	Editable bool
	// Owner is "" (any), BoardsMine or BoardsShared.
	Owner string
	// ProjectUUID keeps the boards linked from the project's home
	// (EntityLink's linked_boards), archived or not: project_deleted_at is
	// the archive mark, and its members still open the project.
	ProjectUUID string
	// TeamUUID keeps the boards linked from any of the team's live projects
	// (team_projects, then linked_boards), as the team page lists them.
	TeamUUID string
}

// GetDgraphBoardList returns the boards a user can access (owned or shared via
// edit/read/comment), newest-edited first, with an optional title search,
// narrowed by filter, and pagination. The narrowing is inside the query, so a
// page is full and the count is the narrowed list's; a deleted board or team,
// a team's archived project and a removed link never count. Relies on the @reverse
// indexes on board_created_by and the board_*_users predicates.
func GetDgraphBoardList(ctx context.Context, userUID string, search string, first int, offset int, filter BoardListFilter) (list *dgraphStruct.DgraphBoardList, err error) {
	variables := make(map[string]string)
	variables["$userId"] = userUID
	variables["$first"] = strconv.Itoa(first)
	variables["$offset"] = strconv.Itoa(offset)
	params := "$userId: string, $first: int, $offset: int"

	conds := []string{dgraphquery.Live("board_deleted_at")}
	if search != "" {
		conds = append(conds, fmt.Sprintf(`regexp(board_title, /%s/i)`, search))
	}

	// Dgraph refuses a query that defines a variable it never uses, so each
	// list defines only the shares it reads: the editable list no read or
	// comment shares, the viewer's own list no shares at all.
	shared := []string{"edits"}
	shares := `edits as ~board_editing_users`
	if !filter.Editable {
		shared = append(shared, "reads", "comments")
		shares += `
					reads as ~board_reading_users
					comments as ~board_commenting_users`
	}
	owned := `owned as ~board_created_by`
	vars, boards := owned+"\n\t\t\t\t\t"+shares, "owned, "+strings.Join(shared, ", ")
	switch filter.Owner {
	case BoardsMine:
		vars, boards = owned, "owned"
	case BoardsShared:
		boards = strings.Join(shared, ", ")
		conds = append(conds, "not uid(owned)")
	}

	// The boards linked from the project, archived or not, or from the live
	// team's live projects, through the links there are now.
	linked := ""
	switch {
	case filter.ProjectUUID != "":
		variables["$scope"] = filter.ProjectUUID
		params += ", $scope: string"
		linked = `var(func: eq(project_uuid, $scope)) {
					linked as linked_boards
				}`
		conds = append(conds, "uid(linked)")
	case filter.TeamUUID != "":
		variables["$scope"] = filter.TeamUUID
		params += ", $scope: string"
		linked = fmt.Sprintf(`var(func: eq(team_uuid, $scope)) @filter(%s) {
					team_projects @filter(%s) {
						linked as linked_boards
					}
				}`, dgraphquery.Live("team_deleted_at"), dgraphquery.Live("project_deleted_at"))
		conds = append(conds, "uid(linked)")
	}
	where := strings.Join(conds, " AND ")

	query := fmt.Sprintf(`query BoardList(%s){
				var(func: uid($userId)) {
					%s
				}
				%s

				board_count(func: uid(%s)) @filter(%s) {
					count(uid)
				}

				boardList(func: uid(%s), orderdesc: board_updated_at, first: $first, offset: $offset) @filter(%s) {
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
			}`, params, vars, linked, boards, where, boards, where)

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
