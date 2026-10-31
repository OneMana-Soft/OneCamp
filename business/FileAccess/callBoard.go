package business

import (
	"context"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/helpers"
)

// CallBoard answers what a call someone is in now gives them on a board: its
// pictures while it is on the call's stage, and adding one while they draw
// on it (calls package B; business/CallState BoardAccess). business/CallState
// sets it: it imports this package, so this one can't import it. Nil gives
// nothing.
var CallBoard func(ctx context.Context, userUUID, boardID string) (read, edit bool, err error)

// boardLive is whether a board is out of the trash, a seam for tests.
var boardLive = boardBusiness.Live

// onStage is a board's own rule (own) for reading its pictures, or adding one
// when edit, widened by a call the person is in now. Their own rights answer
// first. A call gives nothing through an API token, nor on a deleted board.
func onStage(own func(ctx context.Context, userUUID, boardID string) (bool, error), edit bool) func(ctx context.Context, userUUID, boardID string) (bool, error) {
	return func(ctx context.Context, userUUID, boardID string) (bool, error) {
		ok, err := own(ctx, userUUID, boardID)
		if err != nil || ok || CallBoard == nil || helpers.ActsThroughToken(ctx) {
			return ok, err
		}
		if live, err := boardLive(ctx, boardID); err != nil || !live {
			return false, err
		}
		read, draws, err := CallBoard(ctx, userUUID, boardID)
		if edit {
			return draws, err
		}
		return read, err
	}
}
