package controllers

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

func withInviteService(t *testing.T) (*string, *callInviteBusiness.Door) {
	t.Helper()
	oInvite, oEnter := inviteToCall, enterByDoor
	t.Cleanup(func() { inviteToCall, enterByDoor = oInvite, oEnter })
	var room string
	var door callInviteBusiness.Door
	inviteToCall = func(_ context.Context, r string, by *dgraphStruct.DgraphUser) (*guestBusiness.CallLink, error) {
		room = r
		return &guestBusiness.CallLink{GuestToken: "raw", GrantID: "g1", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	enterByDoor = func(_ context.Context, d callInviteBusiness.Door, _ *dgraphStruct.DgraphUser, _ string) (*callInviteBusiness.Entry, error) {
		door = d
		return &callInviteBusiness.Entry{Token: "member-token", Room: "r", Title: "Maya Chen's call"}, nil
	}
	return &room, &door
}

// An invite is made, and a door entered, by the person signed in, in the app:
// never through an API token. Refusals say why in their own words; a link
// that opens nothing answers alike, whatever the reason (HU13).
func TestAnInviteAndADoorAreForThePersonInTheApp(t *testing.T) {
	room, door := withInviteService(t)
	maya := uuid.New()
	channel := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"

	if code, _ := roomAsk(InviteToCall, http.MethodPost, `{"kind":"channel","id":"`+channel+`"}`, nil, false); code != http.StatusUnauthorized {
		t.Errorf("nobody signed in: %d", code)
	}
	if code, out := roomAsk(InviteToCall, http.MethodPost, `{"kind":"channel","id":"`+channel+`"}`, &maya, true); code != http.StatusForbidden || codeOf(out) != "not_through_token" {
		t.Errorf("an API token's invite: %d %v", code, out)
	}
	if code, out := roomAsk(InviteToCall, http.MethodPost, `{"kind":"channel","id":"`+channel+`"}`, &maya, false); code != http.StatusOK || *room != channel || out["data"] == nil {
		t.Errorf("an invite: %d %v (room %q)", code, out, *room)
	}
	if code, _ := roomAsk(InviteToCall, http.MethodPost, `{"kind":"doc","id":"d1"}`, &maya, false); code != http.StatusBadRequest {
		t.Errorf("an invite to a doc: %d", code)
	}
	inviteToCall = func(context.Context, string, *dgraphStruct.DgraphUser) (*guestBusiness.CallLink, error) {
		return nil, callInviteBusiness.ErrNotAllowed
	}
	if code, out := roomAsk(InviteToCall, http.MethodPost, `{"kind":"channel","id":"`+channel+`"}`, &maya, false); code != http.StatusForbidden || codeOf(out) != "not_allowed" {
		t.Errorf("a participant's invite: %d %v", code, out)
	}

	if code, out := roomAsk(EnterByDoor, http.MethodPost, `{"token":"raw","language":"en"}`, &maya, true); code != http.StatusForbidden || codeOf(out) != "not_through_token" {
		t.Errorf("an API token at a door: %d %v", code, out)
	}
	if code, out := roomAsk(EnterByDoor, http.MethodPost, `{"slug":"maya-chen","unlock":"k"}`, &maya, false); code != http.StatusOK || door.Slug != "maya-chen" || door.Unlock != "k" || out["data"] == nil {
		t.Errorf("a room's address: %d %v (%+v)", code, out, *door)
	}
	for name, c := range map[string]struct {
		err  error
		code int
		says string
	}{
		"a passcode first":     {callRoomBusiness.ErrLocked, http.StatusUnauthorized, "locked"},
		"opens nothing":        {guestBusiness.ErrInvalidGrant, http.StatusNotFound, ""},
		"guest access off":     {guestBusiness.ErrGuestDisabled, http.StatusNotFound, ""},
		"put out by a host":    {&helpers.CallRefusal{Code: "removed", Msg: "You were removed from this call."}, http.StatusForbidden, "removed"},
		"a store that blinked": {errors.New("down"), http.StatusServiceUnavailable, ""},
	} {
		err := c.err
		enterByDoor = func(context.Context, callInviteBusiness.Door, *dgraphStruct.DgraphUser, string) (*callInviteBusiness.Entry, error) {
			return nil, err
		}
		if code, out := roomAsk(EnterByDoor, http.MethodPost, `{"token":"raw"}`, &maya, false); code != c.code || codeOf(out) != c.says {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// Someone in the conversation is sent to its call, and nothing is minted here.
	enterByDoor = func(context.Context, callInviteBusiness.Door, *dgraphStruct.DgraphUser, string) (*callInviteBusiness.Entry, error) {
		return &callInviteBusiness.Entry{Room: channel, Title: "Maya Chen's call", Conversation: &callStateBusiness.Ref{Kind: "channel", ID: channel}}, nil
	}
	if code, out := roomAsk(EnterByDoor, http.MethodPost, `{"token":"raw"}`, &maya, false); code != http.StatusOK || out["data"] == nil {
		t.Errorf("a member of the conversation: %d %v", code, out)
	}
}
