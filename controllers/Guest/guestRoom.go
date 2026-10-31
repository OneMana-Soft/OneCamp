package controllers

// A personal room's address (calls CALLS-2, business/CallRoom): where anyone
// outside the workspace comes into a member's room, as a meeting link's token
// routes do, but by the room's address ("/public/r/<slug>"), never a token.
//   - GET /public/r/{slug}?k=: what the address answers now (live, locked),
//     and once live (with the passcode's unlock when it has one) what the
//     meeting is called and has on; a dead address is the uniform 404.
//   - POST /public/r/{slug}/unlock {passcode}: the passcode's unlock.
//   - POST /public/r/{slug}/join?k=: knock, or join at once with the lobby off.
//   - GET and DELETE /public/r/{slug}/knock/{ticket}?k=: the lobby's answer,
//     or giving up waiting.
// Each counts its asks per address under the room kind's surfaces
// (redisStore.AllowPerAddress); a passcode's tries are counted per room
// too. Joining and knocking are the meeting link's own (joinMeeting,
// answerKnock), so a room's guest is let in exactly as a link's is.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/middleware"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/go-chi/chi/v5"
)

// Asks per minute from one address, by what is asked of a room's address. A
// waiting page asks the lobby about twice a minute, each ask held up to 25 s.
const (
	roomReadsPerAddress   = 60
	roomUnlocksPerAddress = 10
	roomJoinsPerAddress   = 20
	roomKnocksPerAddress  = 120
)

// Seams for tests: the room's address, and the per-address limit.
var (
	roomAt        = callRoomBusiness.At
	enterRoom     = callRoomBusiness.Enter
	unlockRoom    = callRoomBusiness.Unlock
	roomAskedFrom = redisStore.AllowPerAddress
)

// roomAsked counts one ask of part of a room's address from the request's
// address, and answers 429 when it is past perMinute, as every public read does.
func roomAsked(w http.ResponseWriter, r *http.Request, part string, perMinute int) bool {
	res := roomAskedFrom(r.Context(), guestBusiness.KindRoom.Surface(part), helpers.ClientIP(r), perMinute)
	if !res.Allowed {
		middleware.WriteRateLimited(w, res, registry.PublicReadRate, "Too many requests from here. Wait a minute and try again.")
	}
	return res.Allowed
}

// roomRead is what a room's page is told: its state, and once live what the
// meeting is called and has on.
func roomRead(ctx context.Context, state string, g *guestModel.GuestGrant) map[string]interface{} {
	if g == nil {
		return map[string]interface{}{"state": state}
	}
	door, err := guestBusiness.DoorOf(ctx, g)
	if err != nil {
		return map[string]interface{}{"state": state}
	}
	read := meetingRead(ctx, door)
	read["state"] = state
	return read
}

// RoomAtAddress GET /public/r/{slug}?k=: what a personal room's address
// answers now. Locked (a passcode, and no unlock that opens it) says only
// that; a dead address (never a room, closed, off) is the uniform 404.
func RoomAtAddress(w http.ResponseWriter, r *http.Request) {
	if !roomAsked(w, r, "", roomReadsPerAddress) {
		return
	}
	ctx := r.Context()
	state, g, err := roomAt(ctx, chi.URLParam(r, "slug"), r.URL.Query().Get("k"))
	switch {
	case err != nil:
		guestReadFailed(w, r, "RoomAtAddress", err, "")
	case state == guestBusiness.StateLive, state == guestBusiness.StateLocked:
		writeGuestRead(w, roomRead(ctx, state, g))
	default:
		notAvailable(w)
	}
}

// UnlockRoom POST /public/r/{slug}/unlock {passcode}: the passcode's unlock,
// carried by the room's page in its asks (?k=) for as long as it lasts.
func UnlockRoom(w http.ResponseWriter, r *http.Request) {
	if !roomAsked(w, r, "unlock", roomUnlocksPerAddress) {
		return
	}
	var in struct {
		Passcode string `json:"passcode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That couldn't be read."})
		return
	}
	token, lasts, err := unlockRoom(r.Context(), chi.URLParam(r, "slug"), in.Passcode, helpers.ClientIP(r))
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"token": token, "expires_in": int(lasts.Seconds())}})
	case errors.Is(err, callRoomBusiness.ErrWrongPasscode):
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"code": "wrong_passcode", "msg": "That passcode doesn't open this room."})
	case errors.Is(err, callRoomBusiness.ErrBusy):
		w.Header().Set("Retry-After", "60")
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"code": "rate_limited", "msg": "Too many tries. Wait a few minutes and try again."})
	default:
		guestReadFailed(w, r, "UnlockRoom", err, "")
	}
}

// entered is the room's link for a join or a knock at its address, having
// answered when there is none: a room with a passcode its page hasn't
// unlocked is 401 "locked", and every other refusal the uniform 404.
func entered(w http.ResponseWriter, r *http.Request, where string) (guestBusiness.Door, bool) {
	g, err := enterRoom(r.Context(), chi.URLParam(r, "slug"), r.URL.Query().Get("k"))
	var door guestBusiness.Door
	if err == nil {
		door, err = guestBusiness.DoorOf(r.Context(), g)
	}
	switch {
	case err == nil:
		return door, true
	case errors.Is(err, callRoomBusiness.ErrLocked):
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"code": "locked", "msg": "Enter the room's passcode to come in."})
	default:
		guestReadFailed(w, r, where, err, "")
	}
	return guestBusiness.Door{}, false
}

// JoinRoom POST /public/r/{slug}/join?k= {display_name, language}: a guest
// knocks on the room, or, with its lobby off, joins at once.
func JoinRoom(w http.ResponseWriter, r *http.Request) {
	if !roomAsked(w, r, "join", roomJoinsPerAddress) {
		return
	}
	var body joinBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	g, ok := entered(w, r, "JoinRoom")
	if !ok {
		return
	}
	joinMeeting(w, r, "JoinRoom", g, body)
}

// RoomKnockAnswer GET and DELETE /public/r/{slug}/knock/{ticket}?k=: what
// came of a guest's knock on a room, or that they gave up waiting. A changed
// passcode ends the unlock they knocked with, so they are asked for it again.
func RoomKnockAnswer(w http.ResponseWriter, r *http.Request) {
	if !roomAsked(w, r, "knock", roomKnocksPerAddress) {
		return
	}
	g, ok := entered(w, r, "RoomKnockAnswer")
	if !ok {
		return
	}
	answerKnock(w, r, "RoomKnockAnswer", g)
}
