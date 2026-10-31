package business

// A board on the call's stage (calls package B: zoom-new-whiteboard, and the
// whiteboard half of meet-10 and zoom-02). One board at a time takes the
// stage's focus, as a screen share does.
//
// WHO MAY (PROGRAM.md, package B, approved 26 Oct with main's change):
//   - Present: anyone from the workspace who may share now (sharing held
//     lets through only the people running the call and whoever they let),
//     and only a board they may edit: a reader can't hand out edit rights. A
//     second presenter waits until the first stops, unless they run the call.
//   - New board: whoever may present. The board is private and shared to
//     edit with the call's conversation's people, read on the server, so
//     what was drawn stays with the conversation once the call ends; a
//     meeting's is its maker's to share. Never the call's guests: they see
//     it only through the call's grant while it is on the stage.
//   - Stop: the presenter and the people running the call; in a DM's or a
//     group's call, which nobody runs, anyone from the workspace in it.
//   - Choose who draws ("Everyone can draw", on by default for the call's
//     members, and "Guests can draw", off by default): the presenter and the
//     people running the call.
//   - See and draw: everyone in the call now, as business/CallState Board.Draws
//     says, through the one board authoriser (BoardAccess).
// Never through an API token, never an agent (Act refuses both first).
//
// WHAT EVERYONE SEES. The board rides in the room's metadata (metaBoard), so
// every screen, late joiners' too, shows the same board. Every change closes
// the board's live sockets (collab.ReauthBoardLater), so each person comes
// back with what they may do now, and someone the call no longer covers
// doesn't come back.

import (
	"context"
	"errors"
	"net/http"
	"slices"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	collab "github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// The board actions.
const (
	ActPresentBoard = "present_board" // board: put it on the stage
	ActNewBoard     = "new_board"     // make a board for the call's conversation and put it on the stage
	ActStopBoard    = "stop_board"
	ActBoardDraw    = "board_draw"   // draw: everyone or presenter
	ActBoardGuests  = "board_guests" // on: the call's guests draw too, or not
	ActUndoGuests   = "undo_guests"  // take back what guests did since they could draw
)

// metaBoard is the room metadata key the board on the stage lives under.
const metaBoard = "board"

// The board refusals.
var (
	ErrGuestBoard   = &Refusal{http.StatusForbidden, "guest_board", "Guests can't present a board. Someone from the workspace can."}
	ErrShareHeld    = &Refusal{http.StatusForbidden, "share_held", "Sharing is held in this call. Ask the people running it to let you share."}
	ErrBoardShown   = &Refusal{http.StatusConflict, "board_shown", "A board is already on the stage. Ask whoever is presenting it to stop first."}
	ErrNoBoard      = &Refusal{http.StatusConflict, "no_board", "There's no board on the stage."}
	ErrNotYourBoard = &Refusal{http.StatusForbidden, "board_not_editable", "You can present only a board you can edit."}
	ErrNoSuchDraw   = &Refusal{http.StatusBadRequest, "no_such_draw", "Drawing is for everyone in the call or only the presenter."}
	ErrNoGuestDraws = &Refusal{http.StatusConflict, "no_guest_drawing", "Guests haven't drawn on this board in the call."}
	ErrBoardBusy    = &Refusal{http.StatusServiceUnavailable, "board_busy", "The board couldn't be reached just now. Try again."}
)

// boardAction is one of the board actions. Pure.
func boardAction(action string) bool {
	return slices.Contains([]string{ActPresentBoard, ActNewBoard, ActStopBoard, ActBoardDraw, ActBoardGuests, ActUndoGuests}, action)
}

// boardRefusal is why me may not do a board action in s, or nil. Pure.
func boardRefusal(s *callStateBusiness.Session, me *callStateBusiness.Person, a Action) error {
	presenter := s.Board != nil && s.Board.By == me.Identity
	runs := model.RunsCall(me.Role)
	switch a.Action {
	case ActPresentBoard, ActNewBoard:
		if me.Guest || me.Role == model.RoleGuest {
			return ErrGuestBoard
		}
		if !mayDo(s, me, model.HoldShare) {
			return ErrShareHeld
		}
		if s.Board != nil && !presenter && !runs {
			return ErrBoardShown
		}
		return nil
	case ActStopBoard:
		if s.Board == nil {
			return ErrNoBoard
		}
		if presenter || runs || (equalCall(s.Resource.Kind) && len(AbilitiesOf(s.Resource.Kind, me.Role)) > 0) {
			return nil
		}
		return ErrNotAllowed
	case ActBoardDraw, ActBoardGuests, ActUndoGuests:
		if s.Board == nil {
			return ErrNoBoard
		}
		if a.Action == ActBoardDraw && a.Draw != callStateBusiness.DrawEveryone && a.Draw != callStateBusiness.DrawPresenter {
			return ErrNoSuchDraw
		}
		if !presenter && !runs {
			return ErrNotAllowed
		}
		return nil
	}
	return ErrUnknownAction
}

// actOnBoard does a board action someone in the call may do (boardRefusal).
func actOnBoard(ctx context.Context, actor *dgraphStruct.DgraphUser, s *callStateBusiness.Session, me *callStateBusiness.Person, a Action) (Result, error) {
	if err := boardRefusal(s, me, a); err != nil {
		return Result{}, err
	}
	switch a.Action {
	case ActPresentBoard, ActNewBoard:
		id, err := boardToPresent(ctx, actor, s, a)
		if err != nil {
			return Result{}, err
		}
		// The room's metadata follows the stage in the order this server
		// changed it (roomLocks); the boards closed are the ones the writes
		// themselves answer, never the ones read before them.
		roomLocks.Acquire(s.Room, 0)
		defer roomLocks.Release(s.Room)
		on, was, err := callStateBusiness.SetBoard(ctx, s, id, me.Identity, model.RunsCall(me.Role))
		if err = settle(err); err != nil {
			return Result{}, err
		}
		if !on {
			return Result{}, ErrBoardShown
		}
		shown := callStateBusiness.Board{UUID: id.String(), By: me.Identity, Draw: callStateBusiness.DrawEveryone}
		if err := tellBoard(ctx, s.Room, &shown); err != nil {
			return Result{}, err
		}
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "board", By: me.Name})
		if was != "" && was != shown.UUID {
			collab.ReauthBoardLater(was, nil)
		}
		collab.ReauthBoardLater(shown.UUID, nil)
		if a.Action == ActNewBoard {
			return Result{Affected: 1, Board: shown.UUID}, nil
		}
		return Result{Affected: 1}, nil
	case ActStopBoard:
		roomLocks.Acquire(s.Room, 0)
		defer roomLocks.Release(s.Room)
		was, err := callStateBusiness.ClearBoard(ctx, s, stopBy(s, me))
		if err = settle(err); err != nil {
			return Result{}, err
		}
		if was == "" {
			return Result{}, ErrNoBoard
		}
		if err := tellBoard(ctx, s.Room, nil); err != nil {
			return Result{}, err
		}
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "board_stopped", By: me.Name})
		collab.ReauthBoardLater(was, nil)
		return Result{Affected: 1}, nil
	case ActUndoGuests:
		roomLocks.Acquire(s.Room, 0)
		defer roomLocks.Release(s.Room)
		return undoGuests(ctx, s.Board.UUID)
	}
	var draw *string
	var guests *bool
	if a.Action == ActBoardDraw {
		draw = &a.Draw
	} else {
		guests = &a.On
	}
	roomLocks.Acquire(s.Room, 0)
	defer roomLocks.Release(s.Room)
	if a.Action == ActBoardGuests && a.On {
		// The version from before guests draw, once per time on the stage:
		// what undo goes back to, however often the switch is turned.
		kept, err := callStateBusiness.GuestsKept(ctx, s)
		if err = settle(err); err != nil {
			return Result{}, err
		}
		if !kept {
			if err := keepForGuests(ctx, s.Board.UUID); err != nil {
				return Result{}, err
			}
			if err := settle(callStateBusiness.MarkGuestsKept(ctx, s)); err != nil {
				return Result{}, err
			}
		}
	}
	now, err := callStateBusiness.SetBoardDraw(ctx, s, draw, guests)
	if err = settle(err); err != nil {
		return Result{}, err
	}
	if now == nil {
		return Result{}, ErrNoBoard
	}
	if err := tellBoard(ctx, s.Room, now); err != nil {
		return Result{}, err
	}
	collab.ReauthBoardLater(now.UUID, nil)
	return Result{Affected: 1}, nil
}

// boardToPresent is the board a present_board puts on the stage, one its
// presenter may edit, or the board a new_board makes for the call.
func boardToPresent(ctx context.Context, actor *dgraphStruct.DgraphUser, s *callStateBusiness.Session, a Action) (uuid.UUID, error) {
	if a.Action == ActNewBoard {
		made, err := newBoardFor(ctx, actor, s)
		if err != nil {
			return uuid.Nil, err
		}
		return uuid.Parse(made)
	}
	id, err := uuid.Parse(a.Board)
	if err != nil {
		return uuid.Nil, ErrNotYourBoard
	}
	board, err := boardBusiness.GetBasicBoardByUUID(ctx, id.String(), actor.Uid)
	if err != nil || !boardBusiness.CanEdit(board, actor.Uuid) {
		return uuid.Nil, ErrNotYourBoard
	}
	return id, nil
}

// Seams for tests: a conversation's people, and making a board.
var (
	conversationPeople = conversationBusiness.PeopleIn
	makeBoard          = boardBusiness.CreateBoard
)

// newBoardFor makes a board for s's call, by actor, named for the call when
// it has a name: private, and shared to edit with the call's conversation's
// people as the graph has them now. A meeting belongs to no conversation, so
// its board is its maker's to share.
func newBoardFor(ctx context.Context, actor *dgraphStruct.DgraphUser, s *callStateBusiness.Session) (string, error) {
	var editors []*dgraphStruct.DgraphUser
	if s.Resource.Kind != callStateBusiness.KindMeeting {
		people, err := conversationPeople(ctx, s.Resource)
		if err != nil {
			return "", err
		}
		editors = sharers(people, actor.Uid)
	}
	made, err := makeBoard(ctx, actor, &boardAdapter.InputCreateBoard{BoardTitle: s.Topic, BoardPrivate: true}, editors...)
	if err != nil {
		return "", err
	}
	return made.Uuid, nil
}

// sharers is who of a conversation's people a board made in its call is
// shared with to edit: everyone who can sign in, each once, but its maker,
// who owns it. Never a bot, an external account (an import's or an
// integration's) or a deactivated person. Pure.
func sharers(people []*dgraphStruct.DgraphUser, maker string) []*dgraphStruct.DgraphUser {
	var out []*dgraphStruct.DgraphUser
	seen := map[string]bool{maker: true}
	for _, p := range people {
		if seen[p.Uid] || p.IsBot || p.IsExternal || helpers.IsSoftDeleted(p.DeletedAt) {
			continue
		}
		seen[p.Uid] = true
		out = append(out, p)
	}
	return out
}

// stopBy is whose board a stop by me may take off: only their own for a
// presenter who doesn't run the call, so a stop racing a host's takeover
// leaves the host's board up; whatever is on the stage ("") for the people
// running the call, and for anyone in a DM's or a group's call. Pure.
func stopBy(s *callStateBusiness.Session, me *callStateBusiness.Person) string {
	if model.RunsCall(me.Role) || equalCall(s.Resource.Kind) {
		return ""
	}
	return me.Identity
}

// keepForGuests keeps the board as it is now, before a call's guests draw on
// it: the live copy's state, stored as a board version (reason 'guests'), and
// only then the live copy's record of guests' strokes starts afresh, so a
// version that couldn't be stored loses nothing.
func keepForGuests(ctx context.Context, board string) error {
	v, err := collab.GuestsSnapshot(ctx, board)
	if err == nil {
		err = boardBusiness.KeepForGuests(ctx, board, v)
	}
	if err == nil {
		err = collab.BeginGuests(ctx, board)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl couldn't keep board %s before guests draw: %+v", board, err)
		return ErrBoardBusy
	}
	return nil
}

// undoGuests takes back what a call's guests did on board since they could
// draw on it, through the board's live copy.
func undoGuests(ctx context.Context, board string) (Result, error) {
	state, err := boardBusiness.GuestsVersion(ctx, board)
	if errors.Is(err, boardBusiness.ErrNoGuestsVersion) {
		return Result{}, ErrNoGuestDraws
	}
	if err != nil {
		return Result{}, err
	}
	n, err := collab.UndoGuests(ctx, board, state)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl couldn't undo guests' drawing on board %s: %+v", board, err)
		return Result{}, ErrBoardBusy
	}
	return Result{Affected: n}, nil
}

// tellBoard writes the board on the stage (nil for none) into the room's
// metadata, which every screen reads, late joiners' too.
func tellBoard(ctx context.Context, room string, b *callStateBusiness.Board) error {
	var meta any
	if b != nil {
		meta = b
	}
	return setMeta(ctx, room, metaBoard, meta)
}

// BoardToken is how a call's guest opens the board on the call's stage.
type BoardToken struct {
	CollabToken  string `json:"collab_token"`
	DocumentName string `json:"document_name"`
	Board        string `json:"board"`
}

// GuestBoardToken answers a call's guest, proved by their own call token, the
// collab token for the board on the stage of the call going on in its room,
// while they are in it now. The collaboration service asks again on every
// connect (guestBusiness.AuthorizeGuestCallBoard), so the token opens nothing
// once the board leaves the stage, they leave, or the call ends.
func GuestBoardToken(ctx context.Context, callToken string) (BoardToken, error) {
	identity, room, err := GuestOf(callToken)
	if err != nil {
		return BoardToken{}, ErrNotInCall
	}
	s, err := guestSession(ctx, room)
	if err != nil {
		return BoardToken{}, err
	}
	if s.Board == nil {
		return BoardToken{}, ErrNoBoard
	}
	me, err := hereNow(ctx, s, identity)
	if err != nil {
		return BoardToken{}, err
	}
	token, err := guestBusiness.IssueCallBoardToken(ctx, identity, me.Name, s.ID, s.Board.UUID)
	if err != nil {
		return BoardToken{}, err
	}
	return BoardToken{CollabToken: token, DocumentName: "board:" + s.Board.UUID, Board: s.Board.UUID}, nil
}
