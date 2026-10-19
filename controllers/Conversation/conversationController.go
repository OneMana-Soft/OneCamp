// Package controllers (Conversation) answers the conversation routes (chat
// A9): sharing history with people added to a DM or group, opening the group
// of the new set, naming and hiding groups, making one a private channel, and
// a conversation's people.
package controllers

import (
	"encoding/json"
	"errors"
	"net/http"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// refOf is the conversation the route names: /conversations/{kind}/{id}.
func refOf(r *http.Request) conversationBusiness.Ref {
	return conversationBusiness.Ref{Kind: chi.URLParam(r, "kind"), ID: chi.URLParam(r, "id")}
}

// who is the signed-in person.
func who(r *http.Request) *userModels.UserInfo {
	u, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return &u
}

// answerFailure answers a conversation change or read that didn't happen: a
// rule's refusal in its words, a conversation that isn't there (or the person
// can't read) as a 404, and anything else as a 503 to try again.
func answerFailure(w http.ResponseWriter, r *http.Request, err error, where string) {
	if rj, ok := sendBusiness.AsRejection(err); ok {
		helpers.WriteJSON(w, rj.Status, helpers.Envolope{"msg": rj.Msg})
		return
	}
	if errors.Is(err, conversationBusiness.ErrNotFound) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Conversation not found"})
		return
	}
	helpers.LogErrorWithContext(r.Context(), "%s: %+v", where, err)
	helpers.WriteRetryLater(w, "That didn't go through.")
}

// decode reads a request's body into v, answering a 400 when it can't.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return false
	}
	return true
}

// GetShareChoices answers what each "Share earlier messages?" choice shares.
func GetShareChoices(w http.ResponseWriter, r *http.Request) {
	choices, err := conversationBusiness.ShareChoices(r.Context(), who(r), refOf(r))
	if err != nil {
		answerFailure(w, r, err, "controllers/GetShareChoices")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": choices})
}

// addPeopleInput is "Add people": who, and how much history they get.
type addPeopleInput struct {
	People []string                   `json:"people"`
	Share  conversationBusiness.Share `json:"share"`
}

// AddPeople opens the conversation of the people there with those added, or
// the one there is for exactly them.
func AddPeople(w http.ResponseWriter, r *http.Request) {
	var in addPeopleInput
	if !decode(w, r, &in) {
		return
	}
	if in.Share == "" {
		in.Share = conversationBusiness.ShareNone
	}
	opened, err := conversationBusiness.AddPeople(r.Context(), who(r), refOf(r), in.People, in.Share)
	if err != nil {
		answerFailure(w, r, err, "controllers/AddPeople")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": opened})
}

// AddParticipantToGroupChat is POST /groupChat/addParticipant as apps already
// loaded call it, over AddPeople: the person goes into the group of the new
// set with no history, and the answer names it as new_grp_id.
func AddParticipantToGroupChat(w http.ResponseWriter, r *http.Request) {
	var in chatAdapter.InputAddParticipantToGroupChat
	if !decode(w, r, &in) {
		return
	}
	from := conversationBusiness.ChatRef(in.GrpUuid)
	opened, err := conversationBusiness.AddPeople(r.Context(), who(r), from, []string{in.UserUuid}, conversationBusiness.ShareNone)
	if err != nil {
		answerFailure(w, r, err, "controllers/AddParticipantToGroupChat")
		return
	}
	msg := "added participant to group chat successfully!"
	if opened.Existed {
		msg = "Group with these participants already exists"
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": msg, "data": helpers.Envolope{"new_grp_id": opened.ID}})
}

// SetHidden hides a DM or group until a newer message comes, or shows it.
func SetHidden(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hidden bool `json:"hidden"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := conversationBusiness.SetHidden(r.Context(), who(r), refOf(r), in.Hidden); err != nil {
		answerFailure(w, r, err, "controllers/SetHidden")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"hidden": in.Hidden}})
}

// Rename names a group, or takes its name away.
func Rename(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := conversationBusiness.Rename(r.Context(), who(r), refOf(r), in.Name); err != nil {
		answerFailure(w, r, err, "controllers/Rename")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"name": in.Name}})
}

// Leave takes the person asking out of a group conversation: POST
// /conversations/group/{id}/leave. A DM, or a group of three, is refused in
// words (hide it instead).
func Leave(w http.ResponseWriter, r *http.Request) {
	if err := conversationBusiness.Leave(r.Context(), who(r), refOf(r)); err != nil {
		answerFailure(w, r, err, "controllers/Leave")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "You left the conversation."})
}

// MakePrivateChannel makes the private channel a DM or group continues in.
func MakePrivateChannel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string   `json:"name"`
		People      []string `json:"people"`
		CopyHistory bool     `json:"copy_history"`
	}
	if !decode(w, r, &in) {
		return
	}
	opened, err := conversationBusiness.MakePrivateChannel(r.Context(), who(r), refOf(r), in.People, in.Name, in.CopyHistory)
	if err != nil {
		answerFailure(w, r, err, "controllers/MakePrivateChannel")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": opened})
}

// GetMembers answers a page of a conversation's people: limit of them, 100
// when it is missing, not a number or below one, and never more than 100
// (conversationBusiness.Members holds its page to maxMembersPage, 100, too).
func GetMembers(w http.ResponseWriter, r *http.Request) {
	limit := helpers.LimitParam(r.URL.Query(), 100, 100)
	people, next, err := conversationBusiness.Members(r.Context(), who(r), refOf(r), r.URL.Query().Get("after"), limit)
	if err != nil {
		answerFailure(w, r, err, "controllers/GetMembers")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"members": people, "next": next}})
}
