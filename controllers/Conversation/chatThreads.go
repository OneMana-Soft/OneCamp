package controllers

import (
	"net/http"
	"strconv"

	chatThreadBusiness "github.com/akashc777/OneCamp/business/ChatThread"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// Threads you follow (chat P2, business/ChatThread): the Threads view's page
// and count, following a thread or not, and reading it.

// GetMyThreads answers a page of the threads the asker follows, unread first,
// from ?offset= (0 for the first).
func GetMyThreads(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	page, err := chatThreadBusiness.Mine(r.Context(), who(r), offset)
	if err != nil {
		answerFailure(w, r, err, "controllers/GetMyThreads")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
}

// GetThreadsUnread answers how many of the threads the asker follows had a
// reply since they last read them.
func GetThreadsUnread(w http.ResponseWriter, r *http.Request) {
	n, err := chatThreadBusiness.UnreadCount(r.Context(), who(r))
	if err != nil {
		answerFailure(w, r, err, "controllers/GetThreadsUnread")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]int{"unread": n}})
}

// PostThreadsReadAll marks every thread the asker follows read, and answers
// how many were unread.
func PostThreadsReadAll(w http.ResponseWriter, r *http.Request) {
	n, err := chatThreadBusiness.ReadAll(r.Context(), who(r))
	if err != nil {
		answerFailure(w, r, err, "controllers/PostThreadsReadAll")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]int{"read": n}})
}

// GetThreadFollow answers whether the asker follows the thread rooted at
// {root}, and their own choice for it.
func GetThreadFollow(w http.ResponseWriter, r *http.Request) {
	choice, err := chatThreadBusiness.ChoiceFor(r.Context(), who(r), chi.URLParam(r, "root"))
	if err != nil {
		answerFailure(w, r, err, "controllers/GetThreadFollow")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": choice})
}

// followInput is "Follow thread" or "Unfollow thread".
type followInput struct {
	Follow string `json:"follow"`
}

// PutThreadFollow follows the thread rooted at {root} whatever the asker's
// part in it ({"follow": "on"}), or never ({"follow": "off"}), and answers
// their standing in it.
func PutThreadFollow(w http.ResponseWriter, r *http.Request) {
	var in followInput
	if !decode(w, r, &in) {
		return
	}
	var on bool
	switch in.Follow {
	case "on":
		on = true
	case "off":
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Follow a thread with on or off."})
		return
	}
	choice, err := chatThreadBusiness.Follow(r.Context(), who(r), chi.URLParam(r, "root"), on)
	if err != nil {
		answerFailure(w, r, err, "controllers/PutThreadFollow")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": choice})
}

// readInput is a thread read, or marked unread.
type readInput struct {
	Read *bool `json:"read"`
}

// PutThreadRead marks the thread rooted at {root} read to now
// ({"read": true}) or its latest reply unread ({"read": false}).
func PutThreadRead(w http.ResponseWriter, r *http.Request) {
	var in readInput
	if !decode(w, r, &in) {
		return
	}
	if in.Read == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say whether the thread is read."})
		return
	}
	root := chi.URLParam(r, "root")
	if err := chatThreadBusiness.SetRead(r.Context(), who(r), root, *in.Read); err != nil {
		answerFailure(w, r, err, "controllers/PutThreadRead")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"root": root, "read": *in.Read}})
}
