package business

import (
	"context"
	"errors"
	"testing"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const callBoardSecret = "test-secret-call-board"

// aCallGuest is a guest's call identity, as door.go mints it.
func aCallGuest() string { return guestIdentityPrefix(uuid.NewString()) + "k3x9" }

func callBoardToken(t *testing.T, identity, session, board string) string {
	t.Helper()
	t.Setenv("JWT_SECRET", callBoardSecret)
	tok, err := IssueCallBoardToken(context.Background(), identity, "Guest: Jane", session, board)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return tok
}

func TestCallBoardTokenCarriesTheGuestTheCallAndTheBoard(t *testing.T) {
	who, session, board := aCallGuest(), uuid.NewString(), uuid.NewString()
	c, err := parseCallBoardClaims(callBoardToken(t, who, session, board), callBoardSecret)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Identity != who || c.Call != session || c.Board != board || c.Name != "Guest: Jane" {
		t.Fatalf("claims = %+v", c)
	}
}

// A call's board token is not a share link: the link path never opens it.
func TestCallBoardTokenNeverOpensThroughALink(t *testing.T) {
	tok := callBoardToken(t, aCallGuest(), uuid.NewString(), uuid.NewString())
	if _, err := parseGuestCollabClaims(tok, callBoardSecret); err == nil {
		t.Fatal("a call's board token parsed as a share link's")
	}
}

// A share link's tokens are not a call's, the raw token on its pictures' path
// and its collab token alike, whether or not this server has its secret: the
// call's path hands them back to the link's, which judges them. With the
// secret, a board shared by link showed none of its pictures (27 Oct 2026).
func TestLinkTokenIsNotACallBoardToken(t *testing.T) {
	t.Setenv("JWT_SECRET", callBoardSecret)
	grant := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceBoard, ResourceID: uuid.NewString()}
	collab, err := IssueGuestCollabToken(context.Background(), grant, "Jane")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	raw, err := randomToken()
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	for _, secret := range []string{callBoardSecret, ""} {
		t.Setenv("JWT_SECRET", secret)
		for name, tok := range map[string]string{"a link's collab token": collab, "a link's raw token": raw, "no token at all": "not.a.token"} {
			if _, err := GuestCallBoardOf(context.Background(), tok); !errors.Is(err, ErrNotCallToken) {
				t.Errorf("%s, secret set %v: err = %v, want ErrNotCallToken", name, secret != "", err)
			}
		}
	}
}

// A token that names a call is the call's path's alone: one this server
// didn't sign is refused there, with or without the server's secret, and
// never handed to the link's path.
func TestACallTokenIsJudgedByTheCallAlone(t *testing.T) {
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"guest": true, "sub": aCallGuest(), "call": uuid.NewString(), "rt": "board", "rid": uuid.NewString(), "exp": 9999999999,
	}).SignedString([]byte("another server's secret"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	t.Setenv("JWT_SECRET", callBoardSecret)
	if _, err := GuestCallBoardOf(context.Background(), forged); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("another server's call token: err = %v, want ErrInvalidGrant", err)
	}
	t.Setenv("JWT_SECRET", "")
	if _, err := GuestCallBoardOf(context.Background(), forged); err == nil || errors.Is(err, ErrNotCallToken) {
		t.Errorf("a call token while the server has no secret: err = %v, want the call's path's own refusal", err)
	}
}

// Only a guest's call identity gets a call's board token: a member opens the
// board through their own session, and a link's collab identity isn't a call's.
func TestCallBoardTokenIsOnlyAGuestsInACall(t *testing.T) {
	t.Setenv("JWT_SECRET", callBoardSecret)
	for _, who := range []string{uuid.NewString(), "0x1a2b", "guest-" + uuid.NewString(), ""} {
		if _, err := IssueCallBoardToken(context.Background(), who, "Jane", uuid.NewString(), uuid.NewString()); err == nil {
			t.Fatalf("issued a call's board token to %q", who)
		}
	}
	signed := func(claims jwt.MapClaims) string {
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(callBoardSecret))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}
	board, session := uuid.NewString(), uuid.NewString()
	for name, claims := range map[string]jwt.MapClaims{
		"member sub":    {"guest": true, "sub": uuid.NewString(), "call": session, "rt": "board", "rid": board, "exp": 9999999999},
		"a doc":         {"guest": true, "sub": aCallGuest(), "call": session, "rt": "doc", "rid": board, "exp": 9999999999},
		"a link's too":  {"guest": true, "sub": aCallGuest(), "call": session, "gid": uuid.NewString(), "rt": "board", "rid": board, "exp": 9999999999},
		"not a guest's": {"sub": aCallGuest(), "call": session, "rt": "board", "rid": board, "exp": 9999999999},
	} {
		if _, err := parseCallBoardClaims(signed(claims), callBoardSecret); !errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("%s: err = %v, want ErrInvalidGrant", name, err)
		}
	}
}

// What a call's guest may do on the board on its stage: refused unless the
// call grants it to them now, and drawing only under "Guests can draw".
func TestCallBoardEdit(t *testing.T) {
	who, session, board := aCallGuest(), uuid.NewString(), uuid.NewString()
	claims := &callBoardClaims{Identity: who, Call: session, Board: board}
	grant := func(draw string, guests bool) *callStateBusiness.Grant {
		return &callStateBusiness.Grant{SessionID: session, Identity: who, Guest: true,
			Board: callStateBusiness.Board{UUID: board, By: "0x1", Draw: draw, Guests: guests}}
	}
	refused := map[string]func() (*callStateBusiness.Grant, string){
		"left the call, or never in it": func() (*callStateBusiness.Grant, string) { return nil, board },
		"another board": func() (*callStateBusiness.Grant, string) {
			return grant(callStateBusiness.DrawEveryone, true), uuid.NewString()
		},
		"another call": func() (*callStateBusiness.Grant, string) {
			g := grant(callStateBusiness.DrawEveryone, true)
			g.SessionID = uuid.NewString()
			return g, board
		},
		"someone else's grant": func() (*callStateBusiness.Grant, string) {
			g := grant(callStateBusiness.DrawEveryone, true)
			g.Identity = aCallGuest()
			return g, board
		},
		"a member's grant": func() (*callStateBusiness.Grant, string) {
			g := grant(callStateBusiness.DrawEveryone, true)
			g.Guest = false
			return g, board
		},
	}
	for name, f := range refused {
		g, b := f()
		if _, err := callBoardEdit(claims, b, g); !errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("%s: err = %v, want ErrInvalidGrant", name, err)
		}
	}
	for _, c := range []struct {
		name   string
		draw   string
		guests bool
		edit   bool
	}{
		{"guests can draw off (the default): read only", callStateBusiness.DrawEveryone, false, false},
		{"guests can draw on", callStateBusiness.DrawEveryone, true, true},
		{"only the presenter draws", callStateBusiness.DrawPresenter, true, false},
	} {
		edit, err := callBoardEdit(claims, board, grant(c.draw, c.guests))
		if err != nil || edit != c.edit {
			t.Fatalf("%s: edit = %v, err = %v; want %v", c.name, edit, err, c.edit)
		}
	}
}
