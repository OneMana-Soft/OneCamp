// Package controllers (Publish) answers the server's own page for a doc anyone
// outside the workspace may read (docs C6, D-25): the public routes for a
// doc's view-only link, by its token or its published address
// (publicPage.go), and a member's copy of such a page into their own docs.
// Publishing itself is the link's (controllers/Guest guestPublish.go).
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Publish"
	"github.com/akashc777/OneCamp/helpers"
)

// Seams for tests: the service.
var duplicate = business.Duplicate

// duplicateInput is the page to copy: its address (or its link's token), and
// on a password page the unlock its address carried.
type duplicateInput struct {
	Slug  string `json:"slug"`
	Token string `json:"token"`
	K     string `json:"k"`
}

// Duplicate POST /guest/links/copy — copies a page into the signed-in
// person's own docs, as the page shows it, when its link lets members copy it.
func Duplicate(w http.ResponseWriter, r *http.Request) {
	user, who := accessBusiness.RequestSubject(r)
	if who.UserUID == "" {
		helpers.WriteUnauthorized(w)
		return
	}
	var in duplicateInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil || (in.Slug == "" && in.Token == "") {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That couldn't be read."})
		return
	}
	id, err := duplicate(r.Context(), &user.UserDgraphInfo, who.AgentUUID, business.Ref{Slug: in.Slug, Token: in.Token}, in.K)
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"doc_uuid": id})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That page isn't there."})
	case errors.Is(err, business.ErrGone):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "This page has expired."})
	case errors.Is(err, business.ErrNoCopies):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Whoever published this page doesn't let it be copied."})
	case errors.Is(err, business.ErrLocked):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Open the page with its password first.", "status": "locked"})
	case errors.Is(err, business.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only a person can copy a page."})
	case errors.Is(err, business.ErrBusy):
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "This page is busy. Try again in a moment.", "status": "rate_limited"})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Publish/Duplicate err: %+v", err)
		helpers.WriteRetryLater(w, "Something went wrong. Try again in a moment.")
	}
}
