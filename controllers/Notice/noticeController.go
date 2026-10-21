// Package controllers (Notice) answers the one-time notices: those for the
// person asking, and their dismissals (business/Notice).
package controllers

import (
	"errors"
	"net/http"

	noticeBusiness "github.com/akashc777/OneCamp/business/Notice"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// Notices is the one-time notices for the person asking that they haven't
// dismissed, in order. GET /notices.
func Notices(w http.ResponseWriter, r *http.Request) {
	me, ok := userModels.FromContext(r.Context())
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	got, err := noticeBusiness.For(r.Context(), noticeBusiness.Person{
		UserID:         me.UserPostgresInfo.Id,
		UserUID:        me.UserDgraphInfo.Uid,
		WorkspaceAdmin: me.UserPostgresInfo.IsAdmin,
	})
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Notices err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't load your notices.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"notices": got}})
}

// DismissNotice records that the person asking has seen a notice, on every
// device: POST /notices/{key}/seen.
func DismissNotice(w http.ResponseWriter, r *http.Request) {
	me, ok := userModels.FromContext(r.Context())
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	err := noticeBusiness.Dismiss(r.Context(), me.UserPostgresInfo.Id, chi.URLParam(r, "key"))
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Dismissed."})
	case errors.Is(err, noticeBusiness.ErrUnknown):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such notice."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/DismissNotice err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't dismiss it.")
	}
}
