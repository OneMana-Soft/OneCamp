package business

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// readBasicBoard is GetBasicBoardByUUID; a test stands in for the graph.
var readBasicBoard = GetBasicBoardByUUID

// ReadableBoard is the board by its uuid as the person may read it: the uuid
// parsed, the board read, its own uuid answering as the one asked for (never
// another board, nor a client's Dgraph uid) and CanRead. Anything else is
// ErrBoardNotFound, so a board one can't open reads as no board; any other
// error is the graph's. The presenter notes' rules and a board's polls
// (business/Poll board.go) read a board through it.
func ReadableBoard(ctx context.Context, boardUUID, userUID, userUUID string) (*dgraphStruct.DgraphBoard, error) {
	id, err := uuid.Parse(boardUUID)
	if err != nil {
		return nil, ErrBoardNotFound
	}
	board, err := readBasicBoard(ctx, id.String(), userUID)
	if errors.Is(err, helpers.ErrNotFound) {
		return nil, ErrBoardNotFound
	}
	if err != nil {
		return nil, err
	}
	if board == nil || board.Uuid != id.String() || !CanRead(board, userUUID) {
		return nil, ErrBoardNotFound
	}
	return board, nil
}

// CanRead is who may read a board: anyone signed in, for a board that isn't
// private; for a private one, its creator and the people it's shared with;
// nobody, once it is deleted. Opening a board, joining its live canvas,
// recording a view of it and its live updates all ask this. Only a board
// read with board_deleted_at can be seen to be deleted (ReadableBoardsFor,
// GetBoardByBoardUUID and GetBasicBoardByUUID read it); one read without it
// reads as live.
func CanRead(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	return board != nil && !isDeleted(board) && couldRead(board, userUUID)
}

// couldRead is CanRead apart from the deletion: whether the person may read
// the board, or could while it was there. A deleted board says it is gone to
// these people only (BoardReadsFor), and to nobody else.
func couldRead(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	if board == nil {
		return false
	}
	if board.IsPrivate == nil || !*board.IsPrivate {
		return true
	}
	return board.HasEditAccess > 0 || board.HasReadAccess > 0 || board.HasCommentAccess > 0 ||
		(board.CreatedBy != nil && board.CreatedBy.Uuid == userUUID)
}

// CanEdit is who may change a board's canvas, and so add a picture to it:
// its creator and the people it's shared with to edit, whether or not it is
// private; nobody, once it is deleted. Everyone else who may read it joins
// the live canvas read-only (BoardCollabAuthorize).
func CanEdit(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	if board == nil || isDeleted(board) {
		return false
	}
	return board.HasEditAccess > 0 || (board.CreatedBy != nil && board.CreatedBy.Uuid == userUUID)
}

// CanPresent is who may present a board: show its frames as slides, put it on
// a call's stage (business/CallControl present_board), and read its frames'
// presenter notes (notes.go). It is the board's edit right, as the web's
// Present asks (hasEditAccess); a member who may only view or comment follows
// a presenter and never reads the notes.
func CanPresent(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	return CanEdit(board, userUUID)
}

// isDeleted is whether a board is deleted. A live board carries the zero time
// (0001-01-01) as its deletion time, so having one is not being deleted: a
// deletion time after 1970 is, as in every board query's filter.
func isDeleted(board *dgraphStruct.DgraphBoard) bool {
	return board != nil && helpers.IsSoftDeleted(board.DeletedAt)
}
