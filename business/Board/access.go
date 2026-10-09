package business

import dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"

// CanRead is who may read a board: anyone signed in, for a board that isn't
// private; for a private one, its creator and the people it's shared with.
// Opening a board, joining its live canvas, recording a view of it and its
// live updates all ask this.
func CanRead(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	if board == nil {
		return false
	}
	if board.IsPrivate == nil || !*board.IsPrivate {
		return true
	}
	return board.HasEditAccess > 0 || board.HasReadAccess > 0 || board.HasCommentAccess > 0 ||
		(board.CreatedBy != nil && board.CreatedBy.Uuid == userUUID)
}
