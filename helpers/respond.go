package helpers

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// UnauthorizedMessage is the one thing a 401 says. Seven wordings were in use
// ("Sign in again.", "Not authorised", "Not signed in", "Unauthorized", ...);
// the web app signs out on any 401, so the words only ever reached a log or a
// person reading the network tab, who deserves one answer.
const UnauthorizedMessage = "Sign in again."

// WriteError answers status with {"msg": msg}, the shape nearly every
// handler already uses ({"error": ...} and {"error": true, "message": ...}
// were the other two).
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, Envolope{"msg": msg})
}

// WriteBadRequest answers 400 with msg.
func WriteBadRequest(w http.ResponseWriter, msg string) { WriteError(w, http.StatusBadRequest, msg) }

// WriteForbidden answers 403 with msg.
func WriteForbidden(w http.ResponseWriter, msg string) { WriteError(w, http.StatusForbidden, msg) }

// WriteNotFound answers 404 with msg.
func WriteNotFound(w http.ResponseWriter, msg string) { WriteError(w, http.StatusNotFound, msg) }

// WriteUnauthorized answers 401 with UnauthorizedMessage.
func WriteUnauthorized(w http.ResponseWriter) {
	WriteError(w, http.StatusUnauthorized, UnauthorizedMessage)
}

// Page is one page of a list: Size rows after the first Index pages.
type Page struct {
	Index, Size int
}

// Offset is how many rows come before the page.
func (p Page) Offset() int { return p.Index * p.Size }

// PageParam reads a list's page from the query: pageIndex (from 0; anything
// else is 0) and pageSize (SizeParam's rules: def when absent or not a number,
// at most max). Twenty handlers parsed pageIndex by hand, each its own way.
func PageParam(q url.Values, def, max int) Page {
	index, err := strconv.Atoi(q.Get("pageIndex"))
	if err != nil || index < 0 {
		index = 0
	}
	return Page{Index: index, Size: SizeParam(q, "pageSize", def, max)}
}

// UUIDParam is the route's uuid parameter name, answering 400 (and false)
// when it isn't one: 127 handlers parsed chi.URLParam by hand.
func UUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		WriteBadRequest(w, "That link isn't right: "+name+" isn't an id.")
		return uuid.Nil, false
	}
	return id, true
}
