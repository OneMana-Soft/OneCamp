package business

// A board's polls (miro-28 to_win 4, "Polls on a slide"): a question on one
// of a board's frames that everyone following the presentation answers on
// that slide, the tally live for every open tab of the board. Stored as every
// poll is (Normalize's caps, poll_votes, the one tally and View), with the
// board and its frame and neither a channel, a post nor a call. Who: the
// board's edit right starts and closes one (business/Board CanEdit); whoever
// may open the board reads it and votes (CanRead), through chat's own routes
// (load asks the board, never a channel); a share-link guest reads the
// question and the tally alone (GuestBoardViews) and never votes. A deleted
// board's polls read as gone, as the board does.

import (
	"context"
	"errors"
	"fmt"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// MaxBoardPolls is how many polls one board may have.
const MaxBoardPolls = 100

var (
	// ErrNotBoardEditor: only who may edit a board starts or closes its polls.
	ErrNotBoardEditor = errors.New("only people who can edit this board start or close its polls")
	// ErrFrameNotOnBoard is a frame the board's stored document doesn't hold.
	ErrFrameNotOnBoard = errors.New("that frame isn't on this board")
)

// BoardPoll is what an editor of a board asks: a poll on one of its frames.
type BoardPoll struct {
	BoardUUID string   `json:"board_uuid"`
	FrameID   string   `json:"frame_id"`
	Question  string   `json:"question"`
	Options   []string `json:"options"`
	Multiple  bool     `json:"multiple"`
}

// Seams for tests: how a poll's change is told.
var (
	publishPoll      = mqttBusiness.PublishPollUpdate
	publishBoardPoll = mqttBusiness.PublishBoardPollUpdate
)

// tell tells every open view of a poll's place that it changed (kind: started
// or changed): a board's poll on the board's own topic, which only who may
// open the board hears (business/MqttAccess readsBoard), never a channel's;
// any other on its channel's or conversation's.
func (w where) tell(p *pollModel.Poll, kind int8) {
	if w.board != "" {
		publishBoardPoll(w.board, p.ID.String(), p.FrameID, kind)
		return
	}
	publishPoll(w.topic(p), p.ID.String())
}

// boardFor is the board a person names by its uuid, when they may open it:
// ErrNoAccess for an id that isn't a board's, a board that isn't there or is
// deleted, or one they may not open. The board is read by its uuid and must
// answer as that board (never a client's Dgraph uid).
func boardFor(ctx context.Context, boardUUID string, user *userModels.UserInfo) (*dgraphStruct.DgraphBoard, error) {
	board, err := boardBusiness.ReadableBoard(ctx, boardUUID, user.UserDgraphInfo.Uid, user.UserDgraphInfo.Uuid)
	if errors.Is(err, boardBusiness.ErrBoardNotFound) {
		return nil, ErrNoAccess
	}
	return board, err
}

// CreateOnBoard starts a poll on a frame of a board, asked by one who may
// edit it; the frame must be on the board's stored document. No post: the
// board's slides show it.
func CreateOnBoard(ctx context.Context, user *userModels.UserInfo, in BoardPoll) (*View, error) {
	n, err := NewPoll{Question: in.Question, Options: in.Options, Multiple: in.Multiple}.Normalize()
	if err != nil {
		return nil, err
	}
	board, err := boardFor(ctx, in.BoardUUID, user)
	if err != nil {
		return nil, err
	}
	if !boardBusiness.CanEdit(board, user.UserDgraphInfo.Uuid) {
		return nil, ErrNotBoardEditor
	}
	on, err := boardBusiness.FrameOnBoard(ctx, board.Uuid, in.FrameID)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, ErrFrameNotOnBoard
	}
	boardID := uuid.MustParse(board.Uuid)
	p := &pollModel.Poll{
		ID: uuid.New(), ChannelID: uuid.Nil, BoardID: &boardID, FrameID: in.FrameID, Question: n.Question,
		Multiple: n.Multiple, Options: optionsOf(n.Options), CreatedBy: user.UserPostgresInfo.Id,
	}
	made, err := pollModel.CreateOnBoard(ctx, p, MaxBoardPolls)
	if err != nil {
		return nil, fmt.Errorf("store poll: %w", err)
	}
	if !made {
		return nil, InputError(fmt.Sprintf("a board can have at most %d polls", MaxBoardPolls))
	}
	w := where{board: board.Uuid}
	helpers.GoSafeNamed("business/Poll CreateOnBoard tell", func() { w.tell(p, mqttStruct.TYPE_CREATE) })
	return view(p, &pollModel.Tally{Counts: map[string]int{}}, user.UserPostgresInfo.Id, true), nil
}

// BoardViews lists a board's polls, oldest first, as a person who may open
// it sees them: their own choices, and whether they may close them.
func BoardViews(ctx context.Context, user *userModels.UserInfo, boardUUID string) ([]*View, error) {
	board, err := boardFor(ctx, boardUUID, user)
	if err != nil {
		return nil, err
	}
	me, edits := user.UserPostgresInfo.Id, boardBusiness.CanEdit(board, user.UserDgraphInfo.Uuid)
	return boardViews(ctx, board.Uuid, me, func(p *pollModel.Poll, t *pollModel.Tally) *View { return view(p, t, me, edits) })
}

// GuestBoardViews lists a board's polls as a guest reads them, for a board
// their link opens now (the caller checked it: business/Guest).
func GuestBoardViews(ctx context.Context, boardUUID string) ([]*View, error) {
	return boardViews(ctx, boardUUID, uuid.Nil, GuestBoardView)
}

func boardViews(ctx context.Context, boardUUID string, voter uuid.UUID, show func(*pollModel.Poll, *pollModel.Tally) *View) ([]*View, error) {
	id, err := uuid.Parse(boardUUID)
	if err != nil {
		return nil, ErrNoAccess
	}
	polls, err := pollModel.OfBoard(ctx, id)
	if err != nil {
		return nil, err
	}
	return viewsOf(ctx, polls, func(*pollModel.Poll) (uuid.UUID, error) { return voter, nil }, show)
}

// GuestBoardView is a board's poll as a share-link guest reads it: the
// question and the tally, never whose a vote is or who asked, and no close.
// Pure.
func GuestBoardView(p *pollModel.Poll, t *pollModel.Tally) *View {
	v := view(p, t, uuid.Nil, false)
	v.Mine, v.CreatedBy = []string{}, ""
	return v
}
