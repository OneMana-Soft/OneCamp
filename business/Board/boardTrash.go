package business

// Deleted boards, and bringing one back (UX-TB-H2-BE, TABLES-BOARDS-13).
//
// A board's delete was already soft (DeleteBoard sets board_deleted_at, search
// marks it and its comments deleted) but nothing listed a deleted board or
// brought one back, so the web said "This can't be undone". Now a person's
// Deleted boards lists the boards they made, and a workspace admin's every
// board that isn't private too; either may bring one back. A private board is
// its maker's alone: admins can't open one (CanRead), and the audit log never
// names one to them. Anyone else is answered as for a missing board, so the
// answer never says a deleted board exists. A board a retention rule removed
// is gone from the graph, and its restore is a 404 (domain's
// RestoreTrashedBoard never writes one back). Comments come back with their
// board: the feed and search read the board's own deletion.

import (
	"context"
	"errors"
	"strconv"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Board"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

var (
	// ErrBoardNotInTrash is a restore of a board that is back already.
	ErrBoardNotInTrash = helpers.Conflict("That board is back already.", nil)
	// ErrBoardTrashCursor is a page of Deleted boards the list never offered.
	ErrBoardTrashCursor = errors.New("that page isn't one the list offered; read it again from the start")
)

// TrashPerson is who made a deleted board, as Deleted boards names them.
type TrashPerson struct {
	UUID       string  `json:"user_uuid"`
	Name       string  `json:"user_name,omitempty"`
	FullName   string  `json:"user_full_name,omitempty"`
	ProfileKey *string `json:"user_profile_object_key,omitempty"`
}

// TrashedBoard is a deleted board as Deleted boards lists it, and a restored
// one as its restore answers (no deletion time then). Mine: the reader made
// it (an admin's list holds others' boards too, which name their maker).
type TrashedBoard struct {
	UUID      string       `json:"board_uuid"`
	Title     string       `json:"board_title"`
	Private   bool         `json:"board_private"`
	DeletedAt *time.Time   `json:"deleted_at,omitempty"`
	Owner     *TrashPerson `json:"owner,omitempty"`
	Mine      bool         `json:"mine"`
}

// TrashPage is one page of Deleted boards; Next, when there are more, is the
// cursor of the page after it.
type TrashPage struct {
	Boards []TrashedBoard `json:"boards"`
	Next   string         `json:"next,omitempty"`
}

// What the trash reaches, seams for tests.
var (
	readBoardTrash   = domain.GetBoardTrash
	readTrashedBoard = domain.GetBasicDgraphBoardByUUID
	restoreTrashed   = domain.RestoreTrashedBoard
	clearBoardSearch = func(ctx context.Context, boardUUID string) error {
		return writeBoardSearch(ctx, boardUUID, map[string]interface{}{"deleted_date": nil})
	}
	uncascadeBoard = opensearchModels.SyncCascadingUnarchiveInOpenSearch
)

// mayRestore is whether a person may bring a deleted board back: its maker,
// or a workspace admin when it isn't private; nobody, a scratch board (one the
// app made and took away again, DiscardBoard). Pure.
func mayRestore(board *dgraphStruct.DgraphBoard, userUUID string, isAdmin bool) bool {
	if board == nil || (board.Scratch != nil && *board.Scratch) {
		return false
	}
	if board.CreatedBy != nil && board.CreatedBy.Uuid != "" && board.CreatedBy.Uuid == userUUID {
		return true
	}
	return isAdmin && (board.IsPrivate == nil || !*board.IsPrivate)
}

// trashedBoardOf is a board as Deleted boards shows it to a person (uuid). Pure.
func trashedBoardOf(b *dgraphStruct.DgraphBoard, userUUID string) TrashedBoard {
	title := b.Title
	if title == "" {
		title = "Untitled board"
	}
	out := TrashedBoard{UUID: b.Uuid, Title: title, Private: b.IsPrivate != nil && *b.IsPrivate, DeletedAt: deletedAt(b)}
	if u := b.CreatedBy; u != nil && u.Uuid != "" {
		out.Owner = &TrashPerson{UUID: u.Uuid, Name: u.UserName, FullName: u.UserFullName, ProfileKey: u.ProfileKey}
		out.Mine = u.Uuid == userUUID
	}
	return out
}

// BoardTrash is a page of the boards a person (graph uid and uuid) may bring
// back, newest deletion first, limit at most, from cursor ("" for the first).
func BoardTrash(ctx context.Context, userUID, userUUID string, isAdmin bool, cursor string, limit int) (*TrashPage, error) {
	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return nil, ErrBoardTrashCursor
		}
		offset = n
	}
	// One more than the page holds says whether more remain past it.
	boards, err := readBoardTrash(ctx, userUID, isAdmin, limit+1, offset)
	if err != nil {
		return nil, err
	}
	page := &TrashPage{Boards: []TrashedBoard{}}
	if len(boards) > limit {
		boards = boards[:limit]
		page.Next = strconv.Itoa(offset + limit)
	}
	for _, b := range boards {
		if b != nil && isDeleted(b) {
			page.Boards = append(page.Boards, trashedBoardOf(b, userUUID))
		}
	}
	return page, nil
}

// trashedFor reads a board in the trash a person may bring back, or answers
// ErrBoardNotFound: no such board, or not theirs to restore. A live board
// they may restore is ErrBoardNotInTrash.
func trashedFor(ctx context.Context, boardUUID, userUID, userUUID string, isAdmin bool) (*dgraphStruct.DgraphBoard, error) {
	b, err := readTrashedBoard(ctx, boardUUID, userUID)
	if errors.Is(err, helpers.ErrNotFound) || (err == nil && (b == nil || b.Uuid == "")) {
		return nil, ErrBoardNotFound
	}
	if err != nil {
		return nil, err
	}
	if !mayRestore(b, userUUID, isAdmin) {
		return nil, ErrBoardNotFound
	}
	if !isDeleted(b) {
		return nil, ErrBoardNotInTrash
	}
	return b, nil
}

// TrashedBoardFor is one board of a person's Deleted boards, for a link to a
// deleted board ("Restore this board"); ErrBoardNotFound when it isn't one.
func TrashedBoardFor(ctx context.Context, boardUUID, userUID, userUUID string, isAdmin bool) (*TrashedBoard, error) {
	b, err := trashedFor(ctx, boardUUID, userUID, userUUID, isAdmin)
	if errors.Is(err, ErrBoardNotInTrash) {
		return nil, ErrBoardNotFound
	}
	if err != nil {
		return nil, err
	}
	out := trashedBoardOf(b, userUUID)
	return &out, nil
}

// RestoreBoard brings a deleted board back for its maker or an admin (graph
// uid and uuid): live in the graph, in search with its comments and its AI
// text, and in the admin audit log. ErrBoardNotFound for anyone else and for
// a board that isn't there (a retention rule's removal included);
// ErrBoardNotInTrash for one back already.
func RestoreBoard(ctx context.Context, boardUUID, userUID, userUUID string, isAdmin bool) (*TrashedBoard, error) {
	b, err := trashedFor(ctx, boardUUID, userUID, userUUID, isAdmin)
	if err != nil {
		return nil, err
	}
	ok, err := restoreTrashed(ctx, boardUUID)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Gone or back since the read: a rule's delete, or another restore, ran in between.
		if _, again := trashedFor(ctx, boardUUID, userUID, userUUID, isAdmin); errors.Is(again, ErrBoardNotInTrash) {
			return nil, ErrBoardNotInTrash
		}
		return nil, ErrBoardNotFound
	}
	settleBoardRestore(ctx, boardUUID)
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.BoardRestored, ResourceKind: KindBoard,
		ResourceUUID: b.Uuid, Actor: boardActor(ctx), At: time.Now(), Data: boardEventData(b)})
	out := trashedBoardOf(b, userUUID)
	out.DeletedAt = nil
	return &out, nil
}

// settleBoardRestore is settleBoardDelete's reverse: the board's entry and
// its comments' unmarked, settled, before the restore answers, so the next
// search finds them; then the AI index, in the background. A write that fails
// is logged: search asks the board itself about every hit (ReadableBoardsFor).
func settleBoardRestore(ctx context.Context, boardUUID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boardSettleTimeout)
	defer cancel()
	if err := clearBoardSearch(ctx, boardUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/settleBoardRestore %s: %v", boardUUID, err)
	}
	if err := uncascadeBoard(ctx, []string{"comment_board_id"}, boardUUID, []string{openSearchStruct.COMMENT_INDEX}, "cascade"); err != nil {
		helpers.LogErrorWithContext(ctx, "business/settleBoardRestore %s comments: %v", boardUUID, err)
	}
	if err := refreshBoardSearch(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "business/settleBoardRestore %s: %v", boardUUID, err)
	}
	un := uncascadeBoard
	helpers.GoSafeNamed("board.restore-ai-search", func() {
		_ = un(context.Background(), []string{"board_uuid"}, boardUUID, []string{boardAIIndex}, "cascade")
	})
}
