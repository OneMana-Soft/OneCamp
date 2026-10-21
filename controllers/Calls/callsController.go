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
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
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

// mayRead answers whether a member may see the calls of what a call belongs
// to: the people who can open its conversation (the Access Resolver's call
// kind), or an instant meeting's own host.
func mayRead(r *http.Request, userInfo models.UserInfo, ref callStateBusiness.Ref) (bool, error) {
	room := callStateBusiness.RoomOf(ref)
	if ref.Kind == callStateBusiness.KindMeeting {
		return liveKitBusiness.HostByRule(r.Context(), room, &userInfo.UserDgraphInfo, false)
	}
	_, err := conversationBusiness.Resolve(r.Context(), &userInfo, conversationBusiness.Ref{Kind: conversationBusiness.KindCall, ID: room})
	if errors.Is(err, conversationBusiness.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
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

// answer writes a call as the member reads it.
func answer(w http.ResponseWriter, r *http.Request, userInfo models.UserInfo, ref callStateBusiness.Ref, s *callStateBusiness.Session) {
	ctx := r.Context()
	room := callStateBusiness.RoomOf(ref)
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
	allowed, err := mayRead(r, userInfo, ref)
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
	answer(w, r, userInfo, ref, s)
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
	allowed, err := mayRead(r, userInfo, s.Resource)
	if err != nil {
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	if !allowed {
		notHere()
		return
	}
	answer(w, r, userInfo, s.Resource, s)
}

// actSummaries says, in the audit log, what each action did.
var actSummaries = map[string]string{
	callControlBusiness.ActMute:         "Muted someone in a call",
	callControlBusiness.ActMuteAll:      "Muted everyone in a call",
	callControlBusiness.ActAskUnmute:    "Asked someone in a call to unmute",
	callControlBusiness.ActAskAllUnmute: "Asked everyone in a call to unmute",
	callControlBusiness.ActRemove:       "Removed someone from a call",
	callControlBusiness.ActLock:         "Locked a call",
	callControlBusiness.ActUnlock:       "Unlocked a call",
	callControlBusiness.ActEnd:          "Ended a call for everyone",
	callControlBusiness.ActMakeCohost:   "Made someone a co-host of a call",
	callControlBusiness.ActRemoveCohost: "Took away someone's co-host part in a call",
	callControlBusiness.ActSpotlight:    "Put someone in the spotlight in a call",
	callControlBusiness.ActEndSpotlight: "Ended the spotlight in a call",
	callControlBusiness.ActMuteOnEntry:  "Changed whether people join a call muted",
}

// ActInCall POST /calls/{session_id}/act — what someone running the call asks
// for: {action, identity, source, on}.
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
	meta := map[string]interface{}{"session": sessionID, "action": a.Action, "affected": result.Affected}
	if a.Identity != "" {
		meta["identity"] = a.Identity
	}
	if a.Action == callControlBusiness.ActMute {
		meta["source"] = a.Source
	}
	if a.Action == callControlBusiness.ActMuteOnEntry {
		meta["on"] = a.On
	}
	auditBusiness.Record(r, "call."+a.Action, auditBusiness.CategoryCalls, actSummaries[a.Action], meta)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": result})
}
