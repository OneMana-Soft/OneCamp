package controllers

// A call's invite and the door members come in by (business/CallInvite,
// router/callsInvites.go; calls HU13):
//   - POST /calls/invite {kind, id}: an invite into the conversation's call
//     going on, for someone in it who may let people in. The raw token is
//     shown once; the web makes it a /guest/m/ link, as a meeting's.
//   - POST /calls/door {token | slug, unlock, language}: a member at a link
//     comes in as themselves: their own call token, or, at a call's invite
//     to a conversation they're in, that conversation's call to join.
// Both in the app, never through an API token: a call is run, and entered,
// by a person.

import (
	"encoding/json"
	"errors"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
)

// Seams for tests: the invite's service.
var (
	inviteToCall = callInviteBusiness.Invite
	enterByDoor  = callInviteBusiness.Enter
)

// inPerson is whether this request is a person in the app, not an API token.
func inPerson(w http.ResponseWriter, r *http.Request) bool {
	if helpers.ActsThroughToken(r.Context()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "not_through_token", "msg": "Calls are joined and run in the app, not through an API token."})
		return false
	}
	return true
}

// InviteToCall POST /calls/invite {kind, id}: an invite into the call going
// on in a channel, a DM or a group.
func InviteToCall(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok || !inPerson(w, r) {
		return
	}
	var body struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	ref, ok := refOf(body.Kind, body.ID)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which conversation's call."})
		return
	}
	room := callStateBusiness.RoomOf(ref)
	link, err := inviteToCall(r.Context(), room, &userInfo.UserDgraphInfo)
	if err != nil {
		if helpers.WriteCallRefusal(w, err) {
			return
		}
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/InviteToCall: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	auditBusiness.Record(r, "call.invite", auditBusiness.CategoryCalls, "Made an invite into a call",
		map[string]interface{}{"room": room, "grant_id": link.GrantID})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": link})
}

// EnterByDoor POST /calls/door {token | slug, unlock, language}: a member at
// a link comes in as themselves.
func EnterByDoor(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok || !inPerson(w, r) {
		return
	}
	var body struct {
		callInviteBusiness.Door
		Language string `json:"language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	entry, err := enterByDoor(r.Context(), body.Door, &userInfo.UserDgraphInfo, body.Language)
	switch {
	case err == nil:
	case errors.Is(err, callRoomBusiness.ErrLocked):
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"code": "locked", "msg": "Enter the room's passcode to come in."})
		return
	case helpers.WriteCallRefusal(w, err):
		return
	case guestBusiness.IsUnavailable(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "This link is no longer available.", "available": false})
		return
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/EnterByDoor: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	if entry.Conversation == nil {
		auditBusiness.Record(r, "call.door.join", auditBusiness.CategoryCalls, "Joined a call by a link, as themselves",
			map[string]interface{}{"room": entry.Room})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": entry})
}
