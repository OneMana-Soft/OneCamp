package business

import (
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CanRead is who may read a board: anyone signed in, for a board that isn't
// private; for a private one, its creator and the people it's shared with;
// nobody, once it is deleted. Opening a board, joining its live canvas,
// recording a view of it and its live updates all ask this. Only a board
// read with board_deleted_at can be seen to be deleted (ReadableBoardsFor
// reads it); one read without it reads as live.
func CanRead(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	if board == nil || isDeleted(board) {
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

// isDeleted is whether a board is deleted. A live board carries the zero time
// (0001-01-01) as its deletion time, so having one is not being deleted: a
// deletion time after 1970 is, as in every board query's filter.
func isDeleted(board *dgraphStruct.DgraphBoard) bool {
	return board != nil && helpers.IsSoftDeleted(board.DeletedAt)
}
