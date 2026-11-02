// Package controllers (WorkFeed) is the Work Change Log's read (WCL-1):
// GET /work/changes?cursor=&limit=, what changed in the caller's work since
// their place in the log, only what they may see (business/WorkFeed). A
// reader asks with no cursor right after reading its lists whole, then from
// each answer's cursor; a 410 tells it to read its lists again.
package controllers

import (
	"errors"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	"github.com/akashc777/OneCamp/helpers"
)

// Changes is GET /work/changes: up to limit changes that the caller may see
// after cursor, in order, and the cursor to ask from next. The limit reads as
// every list's size does (helpers.SizeParam): at most workFeed.MaxSince, which
// a missing or bad one asks for. No cursor answers none and the head.
func Changes(w http.ResponseWriter, r *http.Request) {
	_, who := accessBusiness.RequestSubject(r)
	if who.UserUUID == "" {
		helpers.WriteUnauthorized(w)
		return
	}
	q := r.URL.Query()
	limit := helpers.SizeParam(q, "limit", workFeed.MaxSince, workFeed.MaxSince)
	entries, next, err := workFeed.Changes.Since(r.Context(), who, workFeed.Cursor(q.Get("cursor")), limit)
	switch {
	case errors.Is(err, workFeed.ErrCursorExpired):
		helpers.WriteError(w, http.StatusGone, "That place in the change log is gone: read your lists again.")
		return
	case errors.Is(err, workFeed.ErrBadCursor):
		helpers.WriteBadRequest(w, "That isn't a place in the change log.")
		return
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/WorkFeed Changes err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't read what changed.")
		return
	}
	if entries == nil {
		entries = []workFeed.Entry{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"entries": entries, "cursor": next}})
}
