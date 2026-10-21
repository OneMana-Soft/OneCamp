package controllers

import (
	"net/http"

	draftBusiness "github.com/akashc777/OneCamp/business/Draft"
	"github.com/akashc777/OneCamp/helpers"
)

// GetDrafts answers the asker's drafts, every one, newest first (chat P5).
func GetDrafts(w http.ResponseWriter, r *http.Request) {
	drafts, err := draftBusiness.List(r.Context(), who(r))
	if err != nil {
		answerFailure(w, r, err, "controllers/GetDrafts")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": drafts})
}

// PutDraft keeps a draft at its key, in place of the one there, and answers it
// as kept, with when. A blank draft empties its place.
func PutDraft(w http.ResponseWriter, r *http.Request) {
	var in draftBusiness.Draft
	if !decode(w, r, &in) {
		return
	}
	kept, err := draftBusiness.Put(r.Context(), who(r), in.Key, in)
	if err != nil {
		answerFailure(w, r, err, "controllers/PutDraft")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": kept})
}

// DeleteDraft empties the draft at ?key=: a sent message's, or one cleared.
func DeleteDraft(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if err := draftBusiness.Delete(r.Context(), who(r), key); err != nil {
		answerFailure(w, r, err, "controllers/DeleteDraft")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"key": key}})
}
