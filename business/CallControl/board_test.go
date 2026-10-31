package business

import (
	"context"
	"errors"
	"testing"
	"time"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// A board on a call's stage (calls package B), lower parts first: a guest
// never presents (a board is the workspace's), sharing held holds a board too,
// a second presenter waits unless they run the call, and only the presenter
// and the people running the call stop it or choose who draws. In a DM's or a
// group's call, which nobody runs, anyone from the workspace in it stops it.
func TestABoardOnTheStageIsForWhoMayShare(t *testing.T) {
	host := callStateBusiness.Person{Identity: "0x1", Name: "Maya", Role: model.RoleHost}
	cohost := callStateBusiness.Person{Identity: "0x2", Name: "Jonas", Role: model.RoleCohost}
	member := callStateBusiness.Person{Identity: "0x3", Name: "Sam", Role: model.RoleParticipant}
	other := callStateBusiness.Person{Identity: "0x5", Name: "Ana", Role: model.RoleParticipant}
	guest := callStateBusiness.Person{Identity: "guest-x", Name: "Guest: Priya", Role: model.RoleGuest, Guest: true}
	invitee := callStateBusiness.Person{Identity: "0x6", Name: "Lee", Role: model.RoleInvitee}
	shown := &callStateBusiness.Board{UUID: uuid.NewString(), By: member.Identity, Draw: callStateBusiness.DrawEveryone}

	channel := func(board *callStateBusiness.Board, held ...string) *callStateBusiness.Session {
		s := callWith("channel", host, cohost, member, other, guest, invitee)
		s.Board, s.Held = board, held
		return s
	}
	group := func(board *callStateBusiness.Board) *callStateBusiness.Session {
		s := callWith("group", member, other, guest, invitee)
		s.Board = board
		return s
	}
	for _, c := range []struct {
		name   string
		s      *callStateBusiness.Session
		by     callStateBusiness.Person
		action Action
		want   error
	}{
		{"a guest can't present", channel(nil), guest, Action{Action: ActPresentBoard}, ErrGuestBoard},
		{"a member presents", channel(nil), member, Action{Action: ActPresentBoard}, nil},
		{"held sharing holds a board too", channel(nil, model.HoldShare), member, Action{Action: ActPresentBoard}, ErrShareHeld},
		{"the people running the call present through a hold", channel(nil, model.HoldShare), cohost, Action{Action: ActPresentBoard}, nil},
		{"a second presenter waits", channel(shown), other, Action{Action: ActPresentBoard}, ErrBoardShown},
		{"the presenter shows another board", channel(shown), member, Action{Action: ActPresentBoard}, nil},
		{"a host takes the stage", channel(shown), host, Action{Action: ActPresentBoard}, nil},
		{"nothing to stop", channel(nil), host, Action{Action: ActStopBoard}, ErrNoBoard},
		{"a participant can't stop someone else's board", channel(shown), other, Action{Action: ActStopBoard}, ErrNotAllowed},
		{"a guest can't stop it", channel(shown), guest, Action{Action: ActStopBoard}, ErrNotAllowed},
		{"the presenter stops it", channel(shown), member, Action{Action: ActStopBoard}, nil},
		{"a co-host stops it", channel(shown), cohost, Action{Action: ActStopBoard}, nil},
		{"anyone from the workspace stops it in a group's call", group(shown), other, Action{Action: ActStopBoard}, nil},
		{"a guest can't stop it in a group's call", group(shown), guest, Action{Action: ActStopBoard}, ErrNotAllowed},
		{"an invitee can't stop it in a group's call", group(shown), invitee, Action{Action: ActStopBoard}, ErrNotAllowed},
		{"a participant can't choose who draws", channel(shown), other, Action{Action: ActBoardDraw, Draw: callStateBusiness.DrawPresenter}, ErrNotAllowed},
		{"a guest can't let guests draw", channel(shown), guest, Action{Action: ActBoardGuests, On: true}, ErrNotAllowed},
		{"in a group's call only the presenter chooses who draws", group(shown), other, Action{Action: ActBoardDraw, Draw: callStateBusiness.DrawPresenter}, ErrNotAllowed},
		{"the presenter lets guests draw", channel(shown), member, Action{Action: ActBoardGuests, On: true}, nil},
		{"nothing to undo with no board", channel(nil), host, Action{Action: ActUndoGuests}, ErrNoBoard},
		{"a participant can't undo guests' drawing", channel(shown), other, Action{Action: ActUndoGuests}, ErrNotAllowed},
		{"a guest can't undo guests' drawing", channel(shown), guest, Action{Action: ActUndoGuests}, ErrNotAllowed},
		{"the presenter undoes guests' drawing", channel(shown), member, Action{Action: ActUndoGuests}, nil},
		{"a co-host undoes guests' drawing", channel(shown), cohost, Action{Action: ActUndoGuests}, nil},
		{"a host chooses who draws", channel(shown), host, Action{Action: ActBoardDraw, Draw: callStateBusiness.DrawPresenter}, nil},
		{"drawing is for everyone or the presenter", channel(shown), host, Action{Action: ActBoardDraw, Draw: "guests"}, ErrNoSuchDraw},
		{"no board, no switch", channel(nil), host, Action{Action: ActBoardGuests, On: true}, ErrNoBoard},
		{"a guest can't make a board for the call", channel(nil), guest, Action{Action: ActNewBoard}, ErrGuestBoard},
		{"a member makes a board for the call", channel(nil), member, Action{Action: ActNewBoard}, nil},
		{"held sharing holds a new board too", channel(nil, model.HoldShare), member, Action{Action: ActNewBoard}, ErrShareHeld},
		{"a new board waits for the one on the stage", channel(shown), other, Action{Action: ActNewBoard}, ErrBoardShown},
		{"a host makes a board over the one on the stage", channel(shown), host, Action{Action: ActNewBoard}, nil},
	} {
		if err := boardRefusal(c.s, c.s.Person(c.by.Identity), c.action); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// The board actions are Call Control's own: known, never through an API
// token, and never for someone with no part in a call.
func TestBoardActionsAreRefusedBeforeAnythingIsRead(t *testing.T) {
	for _, a := range []string{ActPresentBoard, ActNewBoard, ActStopBoard, ActBoardDraw, ActBoardGuests, ActUndoGuests} {
		if _, err := Act(context.Background(), nil, uuid.NewString(), Action{Action: a}); !errors.Is(err, ErrNotInCall) {
			t.Errorf("%s with nobody: %v", a, err)
		}
	}
}

// On the stage a board behaves like a screen share (DESIGN-BUILD-21, 26 Oct
// 20:45): it leaves when its presenter leaves the call, and when hosts hold
// sharing, unless its presenter runs the call or was let through; it stays
// while sharing isn't held.
func TestABoardLeavesTheStageAsAShareDoes(t *testing.T) {
	host := callStateBusiness.Person{Identity: "0x1", Name: "Maya", Role: model.RoleHost}
	member := callStateBusiness.Person{Identity: "0x3", Name: "Priya", Role: model.RoleParticipant}
	board := func(by string) *callStateBusiness.Board {
		return &callStateBusiness.Board{UUID: uuid.NewString(), By: by, Draw: callStateBusiness.DrawEveryone}
	}
	call := func(b *callStateBusiness.Board, held []string, people ...callStateBusiness.Person) *callStateBusiness.Session {
		s := callWith("channel", people...)
		s.Board, s.Held = b, held
		return s
	}
	gone := member
	gone.In = false
	letThrough := member
	letThrough.Allowed = []string{model.HoldShare}
	share := []string{model.HoldShare}
	for _, c := range []struct {
		name string
		s    *callStateBusiness.Session
		why  string
	}{
		{"no board", call(nil, share, host, member), ""},
		{"a member's board, sharing open", call(board(member.Identity), nil, host, member), ""},
		{"a host's board, sharing held", call(board(host.Identity), share, host, member), ""},
		{"a let-through member's board, sharing held", call(board(member.Identity), share, host, letThrough), ""},
		{"a member's board, sharing held", call(board(member.Identity), share, host, member), BoardLeftHeld},
		{"its presenter left", call(board(member.Identity), nil, host, member), ""},
		{"a board whose presenter isn't in the call", call(board("0x9"), nil, host, member), BoardLeftPresenter},
	} {
		if c.name == "its presenter left" {
			c.s.People[1] = gone
			c.why = BoardLeftPresenter
		}
		if why := boardLeaves(c.s); why != c.why {
			t.Errorf("%s: %q, want %q", c.name, why, c.why)
		}
	}
}

// A stop takes off only the board the stopper may stop: a presenter who
// doesn't run the call stops their own board, never one a host has put up
// since; the people running the call, and anyone in a DM's or a group's call,
// stop whatever is on the stage.
func TestAStopTakesOffOnlyWhatItMay(t *testing.T) {
	host := callStateBusiness.Person{Identity: "0x1", Name: "Maya", Role: model.RoleHost}
	member := callStateBusiness.Person{Identity: "0x3", Name: "Priya", Role: model.RoleParticipant}
	other := callStateBusiness.Person{Identity: "0x5", Name: "Ana", Role: model.RoleParticipant}
	channel := callWith("channel", host, member)
	group := callWith("group", member, other)
	for _, c := range []struct {
		name string
		s    *callStateBusiness.Session
		me   callStateBusiness.Person
		want string
	}{
		{"a presenter who doesn't run the call", channel, member, member.Identity},
		{"a host", channel, host, ""},
		{"anyone in a group's call", group, other, ""},
	} {
		if got := stopBy(c.s, &c.me); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A board made in a call is shared to edit with the conversation's people
// who can sign in: never a bot, an external account or a deactivated person,
// each once, and never its maker, who owns it.
func TestABoardMadeInACallIsSharedWithItsPeople(t *testing.T) {
	gone := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	never := time.Time{}
	sam := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: uuid.NewString(), DeletedAt: &never}
	ana := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: uuid.NewString(), DeletedAt: &never}
	lee := &dgraphStruct.DgraphUser{Uid: "0x6", Uuid: uuid.NewString()}
	bot := &dgraphStruct.DgraphUser{Uid: "0x3", Uuid: uuid.NewString(), IsBot: true}
	imported := &dgraphStruct.DgraphUser{Uid: "0x4", Uuid: uuid.NewString(), IsExternal: true}
	left := &dgraphStruct.DgraphUser{Uid: "0x5", Uuid: uuid.NewString(), DeletedAt: &gone}
	got := sharers([]*dgraphStruct.DgraphUser{sam, ana, bot, imported, left, ana, lee}, sam.Uid)
	if len(got) != 2 || got[0] != ana || got[1] != lee {
		t.Fatalf("shared with %v, want Ana and Lee", got)
	}
}

// New board makes a private board for the call's conversation, read on the
// server: shared with its people, the maker owning it; a meeting's, which has
// no conversation, is its maker's alone. A conversation that can't be read
// makes no board, so none is made the maker's private board by mistake.
func TestANewBoardIsTheCallsConversations(t *testing.T) {
	sam := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: uuid.NewString()}
	ana := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: uuid.NewString()}
	type made struct {
		input   boardAdapter.InputCreateBoard
		editors []*dgraphStruct.DgraphUser
	}
	var boards []made
	var read []callStateBusiness.Ref
	readFails := false
	restore := conversationPeople
	restoreMake := makeBoard
	t.Cleanup(func() { conversationPeople, makeBoard = restore, restoreMake })
	conversationPeople = func(_ context.Context, ref conversationBusiness.Ref) ([]*dgraphStruct.DgraphUser, error) {
		read = append(read, ref)
		if readFails {
			return nil, errors.New("graph down")
		}
		return []*dgraphStruct.DgraphUser{sam, ana}, nil
	}
	makeBoard = func(_ context.Context, by *dgraphStruct.DgraphUser, input *boardAdapter.InputCreateBoard, editors ...*dgraphStruct.DgraphUser) (*dgraphStruct.DgraphBoard, error) {
		if by != sam {
			t.Errorf("made by %v, want Sam", by)
		}
		boards = append(boards, made{*input, editors})
		return &dgraphStruct.DgraphBoard{Uuid: uuid.NewString()}, nil
	}

	channel := callWith("channel")
	channel.Topic = "Launch review"
	id, err := newBoardFor(context.Background(), sam, channel)
	if err != nil || id == "" {
		t.Fatalf("New board in a channel's call: %q, %v", id, err)
	}
	if len(read) != 1 || read[0] != channel.Resource {
		t.Fatalf("read %v, want the call's channel", read)
	}
	if b := boards[0]; !b.input.BoardPrivate || b.input.BoardTitle != "Launch review" || len(b.editors) != 1 || b.editors[0] != ana {
		t.Fatalf("made %+v, want a private board named for the call, shared with Ana", b)
	}

	meeting := callWith(callStateBusiness.KindMeeting)
	if _, err := newBoardFor(context.Background(), sam, meeting); err != nil {
		t.Fatalf("New board in a meeting: %v", err)
	}
	if len(read) != 1 {
		t.Fatalf("a meeting read a conversation: %v", read)
	}
	if b := boards[1]; !b.input.BoardPrivate || len(b.editors) != 0 {
		t.Fatalf("a meeting's board %+v, want its maker's alone", b)
	}

	readFails = true
	if _, err := newBoardFor(context.Background(), sam, callWith("group")); err == nil || len(boards) != 2 {
		t.Fatalf("a conversation that couldn't be read: %v, %d boards", err, len(boards))
	}
}
