package Calendar

// An event's files over HTTP (gcal-1a head B, business/Calendar eventFiles.go):
// the OneCamp docs and boards it carries, by reference. Both routes are for
// whoever may change the event, and adding is also for someone who may open
// the thing. Each answers the event as its asker now reads it, and fails as
// every calendar route does (writeCalendarError: a thing its adder can't open
// is a 403 in its own words, as it is for a new event made with one).

import (
	"encoding/json"
	"io"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// AddCalendarEventFile puts a doc or a board on an event; one already on it
// stays as it is, and one taken off comes back.
// POST /calendar/events/{eventId}/files {"kind": "doc"|"board", "uuid": …}
func AddCalendarEventFile(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var ref business.FileRef
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&ref); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which doc or board to add: its kind and its id."})
		return
	}
	e, err := business.Events.AddFile(r.Context(), who, chi.URLParam(r, "eventId"), ref)
	if err != nil {
		writeCalendarError(w, r, "AddCalendarEventFile", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": e})
}

// RemoveCalendarEventFile takes a doc or a board off an event. Nothing is
// deleted: the row is marked, and adding it again brings it back. One that
// isn't on the event leaves it as it is.
// DELETE /calendar/events/{eventId}/files/{kind}/{uuid}
func RemoveCalendarEventFile(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	ref := business.FileRef{Kind: chi.URLParam(r, "kind"), UUID: chi.URLParam(r, "uuid")}
	e, err := business.Events.RemoveFile(r.Context(), who, chi.URLParam(r, "eventId"), ref)
	if err != nil {
		writeCalendarError(w, r, "RemoveCalendarEventFile", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": e})
}
