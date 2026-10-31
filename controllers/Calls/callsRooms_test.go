package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func withRoomService(t *testing.T) {
	t.Helper()
	oMine, oOpen, oChange, oClose := myRoom, openRoom, changeRoom, closeRoom
	t.Cleanup(func() { myRoom, openRoom, changeRoom, closeRoom = oMine, oOpen, oChange, oClose })
	fail := func() { t.Helper(); t.Fatal("the room's service was asked") }
	myRoom = func(context.Context, uuid.UUID) (*callRoomBusiness.Room, error) { fail(); return nil, nil }
	openRoom = func(context.Context, uuid.UUID) (*callRoomBusiness.Room, error) { fail(); return nil, nil }
	changeRoom = func(context.Context, uuid.UUID, callRoomBusiness.Changes) (*callRoomBusiness.Room, error) {
		fail()
		return nil, nil
	}
	closeRoom = func(context.Context, uuid.UUID) error { fail(); return nil }
}

// roomAsk calls one of the room's routes as Maya (signed in unless who is
// nil), through an API token when token is set.
func roomAsk(h http.HandlerFunc, method, body string, who *uuid.UUID, token bool) (int, map[string]json.RawMessage) {
	var info *models.UserInfo
	if who != nil {
		info = &models.UserInfo{}
		info.UserDgraphInfo.Uid, info.UserDgraphInfo.Uuid, info.UserPostgresInfo.Id = "0x2a", who.String(), *who
	}
	return askAs(h, httptest.NewRequest(method, "/calls/room", strings.NewReader(body)), info, token)
}

// askAs calls h with r as info's person (signed out when info is nil),
// through an API token when token is set, and answers its status and body.
func askAs(h http.HandlerFunc, r *http.Request, info *models.UserInfo, token bool) (int, map[string]json.RawMessage) {
	ctx := r.Context()
	if info != nil {
		ctx = context.WithValue(ctx, helpers.UserInfoContextKey, *info)
	}
	if token {
		ctx = helpers.WithActsThroughToken(ctx)
	}
	w := httptest.NewRecorder()
	h(w, r.WithContext(ctx))
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func codeOf(out map[string]json.RawMessage) string {
	var c string
	_ = json.Unmarshal(out["code"], &c)
	return c
}

// A room is its owner's: read and changed as the person signed in, never
// someone named in the request, and never through an API token.
func TestARoomIsItsOwnersAlone(t *testing.T) {
	withRoomService(t)
	maya := uuid.New()
	if code, _ := roomAsk(GetMyRoom, http.MethodGet, "", nil, false); code != http.StatusUnauthorized {
		t.Errorf("nobody signed in: %d", code)
	}
	var asked uuid.UUID
	myRoom = func(_ context.Context, owner uuid.UUID) (*callRoomBusiness.Room, error) {
		asked = owner
		return nil, nil
	}
	if code, out := roomAsk(GetMyRoom, http.MethodGet, "", &maya, false); code != http.StatusOK || string(out["data"]) != "null" || asked != maya {
		t.Errorf("no room yet: %d %s (asked for %v)", code, out["data"], asked)
	}
	for _, c := range []struct {
		name   string
		h      http.HandlerFunc
		method string
		body   string
	}{
		{"open", OpenMyRoom, http.MethodPost, ""},
		{"change", ChangeMyRoom, http.MethodPatch, `{"guests_wait":false}`},
		{"close", CloseMyRoom, http.MethodDelete, ""},
	} {
		if code, _ := roomAsk(c.h, c.method, c.body, &maya, true); code != http.StatusForbidden {
			t.Errorf("%s through an API token: %d", c.name, code)
		}
	}
}

// Every refusal is said in words, with a code the page can act on.
func TestARoomsRefusalsAreSaidInWords(t *testing.T) {
	withRoomService(t)
	maya := uuid.New()
	for _, c := range []struct {
		err  error
		code int
		word string
	}{
		{guestBusiness.ErrRoomsOff, http.StatusForbidden, "rooms_off"},
		{guestBusiness.ErrGuestDisabled, http.StatusForbidden, "guests_off"},
		{errors.New("the database is away"), http.StatusServiceUnavailable, ""},
	} {
		openRoom = func(context.Context, uuid.UUID) (*callRoomBusiness.Room, error) { return nil, c.err }
		if code, out := roomAsk(OpenMyRoom, http.MethodPost, "", &maya, false); code != c.code || codeOf(out) != c.word {
			t.Errorf("opening, %v: %d %q", c.err, code, codeOf(out))
		}
	}
	openRoom = func(_ context.Context, owner uuid.UUID) (*callRoomBusiness.Room, error) {
		return &callRoomBusiness.Room{MeetingID: "m1", Address: "/r/maya-chen", GuestsWait: true}, nil
	}
	code, out := roomAsk(OpenMyRoom, http.MethodPost, "", &maya, false)
	var opened callRoomBusiness.Room
	if err := json.Unmarshal(out["data"], &opened); code != http.StatusOK || err != nil || opened.Address != "/r/maya-chen" || !opened.GuestsWait {
		t.Errorf("opened: %d %s", code, out["data"])
	}

	if code, _ := roomAsk(ChangeMyRoom, http.MethodPatch, `{}`, &maya, false); code != http.StatusBadRequest {
		t.Errorf("a change of nothing: %d", code)
	}
	for _, c := range []struct {
		err  error
		code int
		word string
		msg  string
	}{
		{guestBusiness.ErrSlugTaken, http.StatusConflict, "taken", "That address is taken. Try another."},
		{callRoomBusiness.ErrNoRoom, http.StatusNotFound, "no_room", ""},
		{callRoomBusiness.ErrNoAddress, http.StatusConflict, "no_address", ""},
		{&callRoomBusiness.InvalidChange{Reason: "A passcode is 6 to 128 characters."}, http.StatusBadRequest, "", "A passcode is 6 to 128 characters."},
		{&guestBusiness.PublishInvalid{Reason: "An address is 1 to 64 letters, numbers and dashes, starting and ending with a letter or number."}, http.StatusBadRequest, "",
			"An address is 1 to 64 letters, numbers and dashes, starting and ending with a letter or number."},
		{guestBusiness.ErrRoomsOff, http.StatusForbidden, "rooms_off", ""},
		{errors.New("the database is away"), http.StatusServiceUnavailable, "", ""},
	} {
		changeRoom = func(context.Context, uuid.UUID, callRoomBusiness.Changes) (*callRoomBusiness.Room, error) {
			return nil, c.err
		}
		code, out := roomAsk(ChangeMyRoom, http.MethodPatch, `{"slug":"standup"}`, &maya, false)
		var msg string
		_ = json.Unmarshal(out["msg"], &msg)
		if code != c.code || codeOf(out) != c.word || (c.msg != "" && msg != c.msg) {
			t.Errorf("changing, %v: %d %q %q", c.err, code, codeOf(out), msg)
		}
	}
	var got callRoomBusiness.Changes
	changeRoom = func(_ context.Context, _ uuid.UUID, in callRoomBusiness.Changes) (*callRoomBusiness.Room, error) {
		got = in
		return &callRoomBusiness.Room{MeetingID: "m1", Address: "/r/standup", Passcode: true}, nil
	}
	if code, _ := roomAsk(ChangeMyRoom, http.MethodPatch, `{"slug":"standup","passcode":"4321"}`, &maya, false); code != http.StatusOK ||
		got.Slug == nil || *got.Slug != "standup" || got.Passcode == nil || *got.Passcode != "4321" || got.GuestsWait != nil {
		t.Errorf("a change: %d %+v", code, got)
	}

	closeRoom = func(context.Context, uuid.UUID) error { return callRoomBusiness.ErrNoRoom }
	if code, out := roomAsk(CloseMyRoom, http.MethodDelete, "", &maya, false); code != http.StatusNotFound || codeOf(out) != "no_room" {
		t.Errorf("closing no room: %d %q", code, codeOf(out))
	}
	closeRoom = func(context.Context, uuid.UUID) error { return nil }
	if code, _ := roomAsk(CloseMyRoom, http.MethodDelete, "", &maya, false); code != http.StatusOK {
		t.Errorf("closed: %d", code)
	}
}
