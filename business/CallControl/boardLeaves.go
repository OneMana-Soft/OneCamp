package business

// On the stage a board behaves like a screen share (DESIGN-BUILD-21, 26 Oct
// 20:45, PROGRAM.md package B): it leaves when its presenter leaves the call
// (LiveKit's participant_left, which comes after the same reconnect grace
// that ends a screen share, or the sweep's), and when hosts hold sharing
// unless its presenter runs the call or was let through, as their screen
// share is unpublished. Everyone is told why. The board stays in the call's
// report, and a host can put it back; nothing drawn on it is lost.

import (
	"context"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	collab "github.com/akashc777/OneCamp/services/collab"
)

// Why a board left the stage, as the board_stopped packet's reason says it.
const (
	BoardLeftPresenter = "presenter_left"
	BoardLeftHeld      = "share_held"
)

// boardLeaves is why the board on s's stage must leave it now, "" while it
// stays. Pure.
func boardLeaves(s *callStateBusiness.Session) string {
	if s.Board == nil {
		return ""
	}
	p := s.Person(s.Board.By)
	if p == nil || !p.In {
		return BoardLeftPresenter
	}
	if !mayDo(s, p, model.HoldShare) {
		return BoardLeftHeld
	}
	return ""
}

// settleStage takes the board off s's stage when boardLeaves says so, and
// only the board its presenter put there (a host's since stays): the room's
// metadata, a packet saying why, and its sockets closed. The caller holds the
// room's lock (roomLocks), as every board change does.
func settleStage(ctx context.Context, s *callStateBusiness.Session) error {
	why := boardLeaves(s)
	if why == "" {
		return nil
	}
	was, err := callStateBusiness.ClearBoard(ctx, s, s.Board.By)
	if err = settle(err); err != nil || was == "" {
		return err
	}
	if err := tellBoard(ctx, s.Room, nil); err != nil {
		return err
	}
	name := ""
	if p := s.Person(s.Board.By); p != nil {
		name = p.Name
	}
	tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "board_stopped", By: name, Reason: why})
	collab.ReauthBoardLater(was, nil)
	return nil
}
