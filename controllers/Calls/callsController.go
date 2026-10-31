package controllers

// The calls programme's own routes (router/callsSessions.go):
//   - GET /calls/current?kind=&id= : the call going on in a conversation (kind
//     channel, dm or group, id its Ref's id) or an instant meeting (kind
//     meeting, id the meeting's id), as the green room and the conversation's
//     header show it. It replaced the three green-room reads, /ch, /dm and
//     /groupChat callState.
//   - GET /calls/{session_id}: one call, going on or over.
//   - POST /calls/{session_id}/act: what someone running the call asks for
//     (business/CallControl), recorded in the audit log.

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// callAnswer is a call as a member reads it: the call itself (nil when none
// is going on), what is switched on in it, and their own part.
type callAnswer struct {
	Live          bool                                `json:"live"`
	Session       *callStateBusiness.Session          `json:"session"`
	Recording     liveKitBusiness.RecordingView       `json:"recording"`
	Transcription liveKitBusiness.TranscriptionSwitch `json:"transcription"`
	You           youInCall                           `json:"you"`
	// Waiting is who waits in the lobby, for those who may let them in.
	Waiting []callLobbyBusiness.Waiting `json:"waiting,omitempty"`
}

// youInCall is the reader's own part: what they may do, and, when the call
// won't let them in now, why (the green room says it before Join).
type youInCall struct {
	Role    string   `json:"role,omitempty"`
	Can     []string `json:"can"`
	Refusal *refusal `json:"refusal,omitempty"`
}

type refusal struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func signedIn(w http.ResponseWriter, r *http.Request) (models.UserInfo, bool) {
	userInfo, ok := r.Context().Value(helpers.UserInfoContextKey).(models.UserInfo)
	if !ok || strings.TrimSpace(userInfo.UserDgraphInfo.Uid) == "" {
		helpers.WriteUnauthorized(w)
		return userInfo, false
	}
	return userInfo, true
}

// refOf is the call's owner a request names, or false.
func refOf(kind, id string) (callStateBusiness.Ref, bool) {
	kind, id = strings.TrimSpace(kind), strings.TrimSpace(id)
	switch kind {
	case conversationBusiness.KindChannel, conversationBusiness.KindDM, conversationBusiness.KindGroup, callStateBusiness.KindMeeting:
	default:
		return callStateBusiness.Ref{}, false
	}
	ref, _, ok := callStateBusiness.RefOfRoom(callStateBusiness.RoomOf(callStateBusiness.Ref{Kind: kind, ID: id}))
	return ref, ok && ref.Kind == kind
}

// answer writes a call as the member reads it. Someone in by a link reads it
// without what it belongs to (invitee).
func answer(w http.ResponseWriter, r *http.Request, userInfo models.UserInfo, ref callStateBusiness.Ref, s *callStateBusiness.Session, invitee bool) {
	ctx := r.Context()
	room := callStateBusiness.RoomOf(ref)
	if invitee && s != nil {
		c := *s
		c.Resource = callStateBusiness.Ref{}
		s = &c
	}
	out := callAnswer{Session: s, You: youInCall{Can: []string{}}}
	if s != nil {
		out.Live = s.Live
		if p := s.Person(userInfo.UserDgraphInfo.Uid); p != nil && p.In {
			out.You.Role = p.Role
			for _, a := range callControlBusiness.AbilitiesOf(ref.Kind, p.Role) {
				out.You.Can = append(out.You.Can, string(a))
			}
		}
	}
	if out.Live {
		view, err := liveKitBusiness.ReadCallView(ctx, room, false)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/Calls could not read the call in %s: %+v", room, err)
			helpers.WriteRetryLater(w, "The call server couldn't answer just now.")
			return
		}
		out.Recording, out.Transcription = view.Recording, view.Transcription
		meetingHost := ref.Kind == callStateBusiness.KindMeeting
		var refused *helpers.CallRefusal
		if err := liveKitBusiness.MayEnter(ctx, room, &userInfo.UserDgraphInfo, meetingHost); errors.As(err, &refused) {
			out.You.Refusal = &refusal{Code: refused.Code, Msg: refused.Msg}
		}
		if slices.Contains(out.You.Can, string(callControlBusiness.CanAdmit)) {
			waiting, err := callLobbyBusiness.WaitingIn(ctx, room)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "controllers/Calls could not read the lobby of %s: %+v", room, err)
			}
			out.Waiting = waiting
		}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// CurrentCall GET /calls/current?kind=&id= — the call going on in a
// conversation or an instant meeting, for its people; live false when none.
func CurrentCall(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	ref, ok := refOf(r.URL.Query().Get("kind"), r.URL.Query().Get("id"))
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which conversation or meeting."})
		return
	}
	allowed, invitee, err := callStateBusiness.MayRead(r.Context(), &userInfo, ref)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/CurrentCall could not check access: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	if !allowed {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no call here you can see."})
		return
	}
	s, err := callStateBusiness.Current(r.Context(), ref)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/CurrentCall failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	answer(w, r, userInfo, ref, s, invitee)
}

// GetCall GET /calls/{session_id} — one call, going on or over, for the people
// who may see its conversation.
func GetCall(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	s, err := callStateBusiness.Get(r.Context(), chi.URLParam(r, "session_id"))
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/GetCall failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	notHere := func() {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no call here you can see."})
	}
	if s == nil {
		notHere()
		return
	}
	allowed, invitee, err := callStateBusiness.MayRead(r.Context(), &userInfo, s.Resource)
	if err != nil {
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	if !allowed {
		notHere()
		return
	}
	answer(w, r, userInfo, s.Resource, s, invitee)
}

// actSummaries says, in the audit log, what each action did.
var actSummaries = map[string]string{
	callControlBusiness.ActMute:               "Muted someone in a call",
	callControlBusiness.ActMuteAll:            "Muted everyone in a call",
	callControlBusiness.ActAskUnmute:          "Asked someone in a call to unmute",
	callControlBusiness.ActAskAllUnmute:       "Asked everyone in a call to unmute",
	callControlBusiness.ActRemove:             "Removed someone from a call",
	callControlBusiness.ActLock:               "Locked a call",
	callControlBusiness.ActUnlock:             "Unlocked a call",
	callControlBusiness.ActEnd:                "Ended a call for everyone",
	callControlBusiness.ActMakeCohost:         "Made someone a co-host of a call",
	callControlBusiness.ActRemoveCohost:       "Took away someone's co-host part in a call",
	callControlBusiness.ActSpotlight:          "Put someone in the spotlight in a call",
	callControlBusiness.ActEndSpotlight:       "Ended the spotlight in a call",
	callControlBusiness.ActMuteOnEntry:        "Changed whether people join a call muted",
	callControlBusiness.ActLowerHand:          "Lowered someone's raised hand in a call",
	callControlBusiness.ActLowerAll:           "Lowered every raised hand in a call",
	callControlBusiness.ActAdmit:              "Let someone in from a call's lobby",
	callControlBusiness.ActDeny:               "Turned someone away from a call's lobby",
	callControlBusiness.ActAdmitAll:           "Let everyone in from a call's lobby",
	callControlBusiness.ActHold:               "Changed what everyone in a call may do",
	callControlBusiness.ActAllow:              "Changed what one person in a call may do although the call holds it",
	callControlBusiness.ActPresentBoard:       "Put a board on a call's stage",
	callControlBusiness.ActNewBoard:           "Made a board for a call's conversation and put it on the call's stage",
	callControlBusiness.ActStopBoard:          "Took a board off a call's stage",
	callControlBusiness.ActBoardDraw:          "Changed who draws on the board on a call's stage",
	callControlBusiness.ActBoardGuests:        "Changed whether a call's guests draw on the board on its stage",
	callControlBusiness.ActUndoGuests:         "Took back a call's guests' drawing on the board on its stage",
	callControlBusiness.ActBreakoutsSet:       "Set up a call's breakout rooms",
	callControlBusiness.ActBreakoutsAssign:    "Moved someone between a call's breakout rooms",
	callControlBusiness.ActBreakoutsSpread:    "Spread everyone in a call across its breakout rooms",
	callControlBusiness.ActBreakoutsOpen:      "Opened a call's breakout rooms",
	callControlBusiness.ActBreakoutsClose:     "Closed a call's breakout rooms",
	callControlBusiness.ActBreakoutsBroadcast: "Sent a message to every breakout room of a call",
}

// ActInCall POST /calls/{session_id}/act — what someone running the call asks
// for: {action, identity, source, on, what}.
func ActInCall(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	var a callControlBusiness.Action
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	sessionID := chi.URLParam(r, "session_id")
	result, err := callControlBusiness.Act(r.Context(), &userInfo.UserDgraphInfo, sessionID, a)
	var refused *callControlBusiness.Refusal
	if errors.As(err, &refused) {
		helpers.WriteJSON(w, refused.Status, helpers.Envolope{"code": refused.Code, "msg": refused.Msg})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/ActInCall %s failed: %+v", a.Action, err)
		helpers.WriteRetryLater(w, "The call server couldn't do that just now. Try again.")
		return
	}
	// What someone does to themselves (their own hand or feedback) isn't
	// running the call, so it isn't written to the audit log.
	if callControlBusiness.OwnAction(a, userInfo.UserDgraphInfo.Uid) {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": result})
		return
	}
	auditBusiness.Record(r, "call."+a.Action, auditBusiness.CategoryCalls, actSummaries[a.Action], actMeta(sessionID, a, result))
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": result})
}

// actMeta is what the audit log keeps of an action in a call: who and what it
// was done to and how, never what was said (a message to every breakout room
// is counted, not kept). Pure.
func actMeta(sessionID string, a callControlBusiness.Action, result callControlBusiness.Result) map[string]interface{} {
	meta := map[string]interface{}{"session": sessionID, "action": a.Action, "affected": result.Affected}
	if a.Ticket != "" {
		meta["ticket"] = a.Ticket
	}
	if a.Identity != "" {
		meta["identity"] = a.Identity
	}
	if a.Action == callControlBusiness.ActMute {
		meta["source"] = a.Source
	}
	if a.Board != "" {
		meta["board"] = a.Board
	}
	if result.Board != "" {
		meta["board"] = result.Board
	}
	if a.Draw != "" {
		meta["draw"] = a.Draw
	}
	switch a.Action {
	case callControlBusiness.ActMuteOnEntry, callControlBusiness.ActHold, callControlBusiness.ActAllow, callControlBusiness.ActBoardGuests:
		meta["on"] = a.On
	case callControlBusiness.ActBreakoutsSet:
		meta["rooms"] = len(a.Rooms)
	case callControlBusiness.ActBreakoutsAssign:
		meta["room"] = a.Room
	case callControlBusiness.ActBreakoutsOpen:
		meta["choose"] = a.On
		meta["minutes"] = a.Minutes
	}
	if a.What != "" {
		meta["what"] = a.What
	}
	return meta
}

// GuestActInCall POST /guest/calls/act: a guest raising or
// lowering their own hand, or setting their feedback ({action, feedback}),
// proved by their own call token (Authorization: Bearer <token>). Nothing else
// a call does is open to a guest here.
func GuestActInCall(w http.ResponseWriter, r *http.Request) {
	var a callControlBusiness.Action
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	result, err := callControlBusiness.GuestActsOnThemselves(r.Context(), r.Header.Get("Authorization"), a)
	var refused *callControlBusiness.Refusal
	if errors.As(err, &refused) {
		helpers.WriteJSON(w, refused.Status, helpers.Envolope{"code": refused.Code, "msg": refused.Msg})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/GuestActInCall %s failed: %+v", a.Action, err)
		helpers.WriteRetryLater(w, "The call server couldn't do that just now. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": result})
}

// GuestCallBoardToken POST /guest/calls/board-token: the collab token a
// guest opens the board on their call's stage with, proved by their own call
// token (Authorization: Bearer <token>) while they are in the call now.
func GuestCallBoardToken(w http.ResponseWriter, r *http.Request) {
	token, err := callControlBusiness.GuestBoardToken(r.Context(), r.Header.Get("Authorization"))
	var refused *callControlBusiness.Refusal
	if errors.As(err, &refused) {
		helpers.WriteJSON(w, refused.Status, helpers.Envolope{"code": refused.Code, "msg": refused.Msg})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/GuestCallBoardToken failed: %+v", err)
		helpers.WriteRetryLater(w, "The call server couldn't open the board just now. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": token})
}

// SetMeetingLobby POST /calls/meetings/{meeting_id}/lobby {"guests_wait": bool}
// — who waits to be let into a meeting: everyone from outside (the default),
// or nobody (anyone with the link joins at once). The meeting's host only.
func SetMeetingLobby(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	if helpers.ActsThroughToken(r.Context()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Change this in the app, not through an API token."})
		return
	}
	var body struct {
		GuestsWait *bool `json:"guests_wait"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.GuestsWait == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	id := chi.URLParam(r, "meeting_id")
	ref := callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: id}
	host, err := liveKitBusiness.HostByRule(r.Context(), callStateBusiness.RoomOf(ref), &userInfo.UserDgraphInfo, false)
	if err != nil {
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	if !host {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That meeting isn't here."})
		return
	}
	found, err := callLobbyBusiness.SetLobby(r.Context(), id, *body.GuestsWait)
	if err != nil {
		helpers.WriteRetryLater(w, "The server couldn't change that just now.")
		return
	}
	if !found {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That meeting isn't here."})
		return
	}
	auditBusiness.Record(r, "call.lobby", auditBusiness.CategoryCalls, "Changed who waits in a meeting's lobby", map[string]interface{}{"meeting": id, "guests_wait": *body.GuestsWait})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"guests_wait": *body.GuestsWait}})
}
