package controllers

// A call's polls and Q&A (calls package A, "Ask the room": meet-10, zoom-02),
// read and acted on by a member through their session, and by a guest through
// their own call token. What whoever runs the activities does is written to
// the audit log; a vote, a question or an upvote never is, so nobody's choice
// or question, anonymous or not, is recorded there.

import (
	"encoding/json"
	"errors"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// activitySummaries are the audit log's words for what whoever runs a call's
// polls and Q&A does.
var activitySummaries = map[string]string{
	callControlBusiness.OpPoll:      "Started a poll in a call",
	callControlBusiness.OpClosePoll: "Closed a poll in a call",
	callControlBusiness.OpShare:     "Shared a poll's results in a call",
	callControlBusiness.OpQA:        "Changed how questions work in a call",
	callControlBusiness.OpQuestion:  "Answered, pinned or dismissed a question in a call",
}

// CallActivities GET /calls/{session_id}/activities: the call's polls and
// questions as the member reads them.
func CallActivities(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	a, err := callControlBusiness.ActivitiesFor(r.Context(), &userInfo.UserDgraphInfo, chi.URLParam(r, "session_id"))
	if refusedActivity(w, r, "CallActivities", err) {
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": a})
}

// DoCallActivity POST /calls/{session_id}/activities: {op, …}
// (callControlBusiness.Activity). Answers the id of a poll started or a
// question asked.
func DoCallActivity(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	var a callControlBusiness.Activity
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	sessionID := chi.URLParam(r, "session_id")
	id, err := callControlBusiness.DoActivity(r.Context(), &userInfo.UserDgraphInfo, sessionID, a)
	if refusedActivity(w, r, "DoCallActivity "+a.Op, err) {
		return
	}
	if callControlBusiness.RunsActivities(a.Op) {
		auditBusiness.Record(r, "call.activities."+a.Op, auditBusiness.CategoryCalls, activitySummaries[a.Op], activityAudit(sessionID, id, a))
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"id": id}})
}

// activityAudit is what the audit log keeps of something whoever runs a
// call's activities did: the call, what, and which poll or question, never
// a question's words or anyone who voted or asked. Pure.
func activityAudit(sessionID, id string, a callControlBusiness.Activity) map[string]interface{} {
	meta := map[string]interface{}{"session": sessionID, "op": a.Op}
	if id == "" {
		id = a.ID
	}
	if id != "" {
		meta["id"] = id
	}
	switch {
	case a.Poll != nil && a.Op == callControlBusiness.OpPoll:
		meta["anonymous"], meta["hide_results"] = a.Poll.Anonymous, a.Poll.HideResults
	case a.QA != nil && a.Op == callControlBusiness.OpQA:
		meta["open"], meta["shown"], meta["anonymous"] = a.QA.Open, a.QA.Shown, a.QA.Anonymous
	case a.Op == callControlBusiness.OpQuestion:
		if a.State != "" {
			meta["state"] = a.State
		}
		if a.Pinned != nil {
			meta["pinned"] = *a.Pinned
		}
	}
	return meta
}

// GuestCallActivities GET /guest/calls/activities: the call's polls and
// questions as a guest in it reads them, by their own call token
// (Authorization: Bearer <token>).
func GuestCallActivities(w http.ResponseWriter, r *http.Request) {
	a, err := callControlBusiness.GuestActivities(r.Context(), r.Header.Get("Authorization"))
	if refusedActivity(w, r, "GuestCallActivities", err) {
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": a})
}

// GuestDoCallActivity POST /guest/calls/activities: a guest votes, asks or
// upvotes ({op, …}), by their own call token. Nothing whoever runs the call
// does is open to a guest.
func GuestDoCallActivity(w http.ResponseWriter, r *http.Request) {
	var a callControlBusiness.Activity
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	id, err := callControlBusiness.GuestDoesActivity(r.Context(), r.Header.Get("Authorization"), a)
	if refusedActivity(w, r, "GuestDoCallActivity "+a.Op, err) {
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"id": id}})
}

// refusedActivity answers a call's activities' refusal in its own words, or
// any other failure as one to try again, and says whether it answered.
func refusedActivity(w http.ResponseWriter, r *http.Request, what string, err error) bool {
	if err == nil {
		return false
	}
	var refused *callControlBusiness.Refusal
	if errors.As(err, &refused) {
		helpers.WriteJSON(w, refused.Status, helpers.Envolope{"code": refused.Code, "msg": refused.Msg})
		return true
	}
	helpers.LogErrorWithContext(r.Context(), "controllers/Calls/%s failed: %+v", what, err)
	helpers.WriteRetryLater(w, "The call couldn't do that just now. Try again.")
	return true
}
