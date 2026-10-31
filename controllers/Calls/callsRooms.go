package controllers

// A member's personal meeting room (business/CallRoom, router/callsRooms.go):
//   - GET /calls/room: their room, or null when they have none.
//   - POST /calls/room: open it (the one they have, or a new one at their name).
//   - PATCH /calls/room {slug?, passcode?, guests_wait?}: change it.
//   - DELETE /calls/room: close it; its address stops answering at once.
// Only its owner reads or changes it; an admin turns its link off in the
// guest links list, or every room off with "Personal meeting rooms". Guests
// come in at its address, on controllers/Guest's public room routes.

import (
	"encoding/json"
	"errors"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
)

// Seams for tests: the room's service.
var (
	myRoom     = callRoomBusiness.Mine
	openRoom   = callRoomBusiness.Open
	changeRoom = callRoomBusiness.Change
	closeRoom  = callRoomBusiness.Close
)

// GetMyRoom GET /calls/room: the member's room, null when they have none.
func GetMyRoom(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	room, err := myRoom(r.Context(), userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/GetMyRoom: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": room})
}

// roomWrite is whether a member may open, change or close their room from
// this request: in the app, not through an API token.
func roomWrite(w http.ResponseWriter, r *http.Request) bool {
	if helpers.ActsThroughToken(r.Context()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Change your room in the app, not through an API token."})
		return false
	}
	return true
}

// roomRefused answers what the workspace's switches say about rooms, and
// reports whether err was one of them.
func roomRefused(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, guestBusiness.ErrGuestDisabled):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "guests_off", "msg": "Guest access is off for this workspace, so personal rooms are too."})
	case errors.Is(err, guestBusiness.ErrRoomsOff):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "rooms_off", "msg": "Personal rooms are off for this workspace."})
	default:
		return false
	}
	return true
}

// OpenMyRoom POST /calls/room: the member's room, opened: the one they have,
// or a new one at their name.
func OpenMyRoom(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok || !roomWrite(w, r) {
		return
	}
	room, err := openRoom(r.Context(), userInfo.UserPostgresInfo.Id)
	if roomRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/OpenMyRoom: %+v", err)
		helpers.WriteRetryLater(w, "Your room couldn't be opened just now. Try again.")
		return
	}
	auditBusiness.Record(r, "call.room.open", auditBusiness.CategoryCalls, "Opened their personal meeting room",
		map[string]interface{}{"meeting": room.MeetingID, "address": room.Address})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": room})
}

// ChangeMyRoom PATCH /calls/room {slug?, passcode?, guests_wait?}: each only
// when sent; all of them or none.
func ChangeMyRoom(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok || !roomWrite(w, r) {
		return
	}
	var in callRoomBusiness.Changes
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Slug == nil && in.Passcode == nil && in.GuestsWait == nil) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	room, err := changeRoom(r.Context(), userInfo.UserPostgresInfo.Id, in)
	var invalid *callRoomBusiness.InvalidChange
	var unpublishable *guestBusiness.PublishInvalid
	switch {
	case err == nil:
	case roomRefused(w, err):
		return
	case errors.Is(err, callRoomBusiness.ErrNoRoom):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "no_room", "msg": "You don't have a room yet."})
		return
	case errors.Is(err, callRoomBusiness.ErrNoAddress):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "no_address", "msg": "An admin turned your room's link off. Open your room again to give it an address."})
		return
	case errors.Is(err, guestBusiness.ErrSlugTaken):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "taken", "msg": "That address is taken. Try another."})
		return
	case errors.As(err, &invalid):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": invalid.Reason})
		return
	case errors.As(err, &unpublishable):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": unpublishable.Reason})
		return
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/ChangeMyRoom: %+v", err)
		helpers.WriteRetryLater(w, "Your room couldn't be changed just now. Try again.")
		return
	}
	// What changed, never the passcode itself.
	changed := map[string]interface{}{"meeting": room.MeetingID}
	if in.Slug != nil {
		changed["address"] = room.Address
	}
	if in.Passcode != nil {
		changed["passcode"] = *in.Passcode != ""
	}
	if in.GuestsWait != nil {
		changed["guests_wait"] = *in.GuestsWait
	}
	auditBusiness.Record(r, "call.room.change", auditBusiness.CategoryCalls, "Changed their personal meeting room", changed)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": room})
}

// CloseMyRoom DELETE /calls/room: the room's address stops answering, its
// guests are put out of its call, and opening again makes a new room.
func CloseMyRoom(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok || !roomWrite(w, r) {
		return
	}
	err := closeRoom(r.Context(), userInfo.UserPostgresInfo.Id)
	switch {
	case errors.Is(err, callRoomBusiness.ErrNoRoom):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "no_room", "msg": "You don't have a room."})
		return
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/CloseMyRoom: %+v", err)
		helpers.WriteRetryLater(w, "Your room couldn't be closed just now. Try again.")
		return
	}
	auditBusiness.Record(r, "call.room.close", auditBusiness.CategoryCalls, "Closed their personal meeting room", nil)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"closed": true}})
}
