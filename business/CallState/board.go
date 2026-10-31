package business

// The board on a call's stage (calls package B): one board at a time, put there
// by someone in the call, drawn on by everyone from the workspace by default,
// and by the call's guests only under the presenter's "Guests can draw". What
// a call gives on a board (Grant) lasts while the board is on the stage of a
// live call and the person is in it; nothing of it outlasts either.

import (
	"context"
	"slices"
	"strings"

	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// Who draws on the board on a call's stage.
const (
	DrawEveryone  = model.DrawEveryone
	DrawPresenter = model.DrawPresenter
)

// Board is the board on a call's stage.
type Board struct {
	UUID string `json:"uuid"`
	// By is the presenter's call identity.
	By string `json:"by"`
	// Draw is who draws on it: DrawEveryone or DrawPresenter.
	Draw string `json:"draw"`
	// Guests is whether the call's guests draw too ("Guests can draw").
	Guests bool `json:"guests"`
}

// Draws is whether someone in the call draws on the board: the presenter
// always; anyone else only while everyone draws, and a guest only while
// guests may too. Pure.
func (b Board) Draws(identity string, guest bool) bool {
	if identity == b.By {
		return true
	}
	return b.Draw == DrawEveryone && (!guest || b.Guests)
}

// boardOf is the board on a session row's stage, or nil. Pure.
func boardOf(s *model.Session) *Board {
	if s.BoardUUID == nil {
		return nil
	}
	return &Board{UUID: s.BoardUUID.String(), By: s.BoardBy, Draw: s.BoardDraw, Guests: s.BoardGuestsDraw}
}

// Grant is what a call gives someone in it now on the board on its stage.
type Grant struct {
	SessionID string
	Board     Board
	// Identity is who they are in the call: a member's graph uid, or a guest's
	// "guest-…" identity.
	Identity string
	Guest    bool
}

// BoardAccess is what someone may do on a board: its own rights (canRead,
// canEdit), and what a call they are in now adds (grant, nil for none). A
// call adds nothing through an API token. Pure.
func BoardAccess(canRead, canEdit bool, grant *Grant, throughToken bool) (read, edit bool) {
	if grant == nil || throughToken {
		return canRead, canRead && canEdit
	}
	return true, canEdit || grant.Board.Draws(grant.Identity, grant.Guest)
}

// BoardGrant answers what a call gives identity on board: nil unless the board
// is on the stage of a live call they are in now, as Call State records it and
// LiveKit, which has the last word, sees it. An agent is given nothing.
func BoardGrant(ctx context.Context, board, identity string) (*Grant, error) {
	id, err := uuid.Parse(strings.TrimSpace(board))
	identity = strings.TrimSpace(identity)
	if err != nil || identity == "" || livekitInit.IsAgentIdentity(identity) {
		return nil, nil
	}
	stages, err := model.BoardStagesOf(ctx, id, identity)
	if err != nil {
		return nil, err
	}
	return grantOn(ctx, id, stages)
}

// MemberBoardGrant is BoardGrant for a member, by their user uuid: how File
// Access, which knows people by it, asks.
func MemberBoardGrant(ctx context.Context, board, userUUID string) (*Grant, error) {
	id, err := uuid.Parse(strings.TrimSpace(board))
	if err != nil {
		return nil, nil
	}
	user, err := uuid.Parse(strings.TrimSpace(userUUID))
	if err != nil {
		return nil, nil
	}
	stages, err := model.BoardStagesOfMember(ctx, id, user)
	if err != nil {
		return nil, err
	}
	return grantOn(ctx, id, stages)
}

// grantOn is the grant from the first of stages (live calls with board on
// their stage, each with someone in it by Call State's record) that LiveKit
// also sees them in. Never an agent's.
func grantOn(ctx context.Context, board uuid.UUID, stages []model.BoardStage) (*Grant, error) {
	stages = slices.DeleteFunc(stages, func(st model.BoardStage) bool { return livekitInit.IsAgentIdentity(st.Identity) })
	for _, st := range stages {
		here, err := livekitInit.LiveKitService.RoomHasParticipant(ctx, st.Room, st.Identity)
		if err != nil {
			return nil, err
		}
		if here {
			return &Grant{
				SessionID: st.SessionID.String(),
				Board:     Board{UUID: board.String(), By: st.By, Draw: st.Draw, Guests: st.GuestsDraw},
				Identity:  st.Identity,
				Guest:     st.Guest,
			}, nil
		}
	}
	return nil, nil
}

func init() {
	fileAccessBusiness.CallBoard = memberBoardAccess
}

// memberBoardAccess is what a call a member is in now gives them on a board,
// for its pictures (File Access).
func memberBoardAccess(ctx context.Context, userUUID, board string) (bool, bool, error) {
	grant, err := MemberBoardGrant(ctx, board, userUUID)
	if err != nil {
		return false, false, err
	}
	read, edit := BoardAccess(false, false, grant, false)
	return read, edit, nil
}

// SetBoard puts board on the call's stage for by (their call identity), with
// everyone drawing and guests not. Unless takeover, only while the stage has
// no board or by's own. Answers whether it went on, and the board it took the
// stage from ("" for none). ErrCallOver once it ended.
func SetBoard(ctx context.Context, s *Session, board uuid.UUID, by string, takeover bool) (bool, string, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return false, "", err
	}
	on, was, err := model.SetBoard(ctx, row.ID, board, by, takeover)
	return on, idOrEmpty(was), err
}

// ClearBoard takes the board off the call's stage, only if by put it there
// unless by is "", answering the board it took off ("" when none was).
// ErrCallOver once it ended.
func ClearBoard(ctx context.Context, s *Session, by string) (string, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return "", err
	}
	was, err := model.ClearBoard(ctx, row.ID, by)
	return idOrEmpty(was), err
}

// SetBoardDraw sets who draws on the board on the call's stage (draw, unless
// nil) and whether its guests do (guests, unless nil), each on its own.
// Answers the board as it is now, nil when none was on the stage. ErrCallOver
// once it ended.
func SetBoardDraw(ctx context.Context, s *Session, draw *string, guests *bool) (*Board, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return nil, err
	}
	b, on, err := model.SetBoardDraw(ctx, row.ID, draw, guests)
	if err != nil || !on {
		return nil, err
	}
	return &Board{UUID: b.UUID.String(), By: b.By, Draw: b.Draw, Guests: b.GuestsDraw}, nil
}

// GuestsKept is whether the board on the call's stage had its version from
// before guests drew kept in its time on the stage.
func GuestsKept(ctx context.Context, s *Session) (bool, error) {
	row, err := rowOf(ctx, s)
	if err != nil {
		return false, err
	}
	return model.GuestsKept(ctx, row.ID)
}

// MarkGuestsKept records that it was. ErrCallOver once the call ended.
func MarkGuestsKept(ctx context.Context, s *Session) error {
	row, err := rowOf(ctx, s)
	if err != nil {
		return err
	}
	_, err = model.MarkGuestsKept(ctx, row.ID)
	return err
}

// idOrEmpty is an id, "" for uuid.Nil. Pure.
func idOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
