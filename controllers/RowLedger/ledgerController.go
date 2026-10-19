package controllers

// HTTP handlers for the row ledger (tables A2, design/tables-vs-airtable/
// PROGRAM.md §4.2 #62): batches of rows, moving rows, undo and redo, a row's
// history, and the trash for rows, fields and tables. Mounted on /tables
// behind the member group (router/tablesLedger.go); who may do what is the
// business layer's, as for every table route.

import (
	"encoding/json"
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func actorFrom(r *http.Request) business.Actor {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return business.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
	}
}

func idParam(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	return id, err == nil
}

// beforeParam is a page's ?before=, the last item of the page before; nil
// for the first page, or for one that isn't an id.
func beforeParam(r *http.Request) *uuid.UUID {
	if id, err := uuid.Parse(r.URL.Query().Get("before")); err == nil {
		return &id
	}
	return nil
}

// writeErr answers a refused write or read: a batch's rows' errors (400,
// with each), a row changed since it was read (409, with the row as it is),
// nothing to undo or redo (409), no access (403), not there (404), or why.
func writeErr(w http.ResponseWriter, err error) {
	var rows business.BatchErrors
	var conflict *business.RowConflict
	switch {
	case errors.As(err, &rows):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": rows.Error(), "errors": []business.BatchError(rows)})
	case errors.As(err, &conflict):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": conflict.Error(), "row": conflict.Row})
	case business.IsConflict(err):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": err.Error()})
	case business.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you don't have access to this table"})
	case business.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "table not found"})
	case business.IsRowNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "row not found"})
	case business.IsUnavailable(err):
		helpers.WriteRetryLater(w, "Couldn't load the rows.")
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

// tableID is the route's table, or an answer saying it isn't one.
func tableID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
	}
	return id, ok
}

// decode reads a request's JSON body into into, or answers that it can't.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return false
	}
	return true
}

// Batch POST /tables/{id}/rows/batch {create, at, patch, delete, kind}: up to
// 1,000 rows in one change set.
func Batch(w http.ResponseWriter, r *http.Request) {
	id, ok := tableID(w, r)
	if !ok {
		return
	}
	var in business.BatchInput
	if !decode(w, r, &in) {
		return
	}
	out, err := business.Batch(business.WithZone(r.Context(), r.URL.Query().Get("tz")), id, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// MoveRows POST /tables/{id}/rows/move {rows, after | before}.
func MoveRows(w http.ResponseWriter, r *http.Request) {
	id, ok := tableID(w, r)
	if !ok {
		return
	}
	var in struct {
		Rows []uuid.UUID `json:"rows"`
		business.PlaceInput
	}
	if !decode(w, r, &in) {
		return
	}
	cs, err := business.MoveRows(r.Context(), id, in.Rows, in.PlaceInput, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"change_set": cs}})
}

// Undo POST /tables/{id}/undo {change_set?}: the person's own change, the one
// named or their latest.
func Undo(w http.ResponseWriter, r *http.Request) {
	id, ok := tableID(w, r)
	if !ok {
		return
	}
	var in struct {
		ChangeSet *uuid.UUID `json:"change_set"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	out, err := business.Undo(r.Context(), id, in.ChangeSet, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// Redo POST /tables/{id}/redo: the person's latest undo, redone.
func Redo(w http.ResponseWriter, r *http.Request) {
	id, ok := tableID(w, r)
	if !ok {
		return
	}
	out, err := business.Redo(r.Context(), id, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// RowHistory GET /tables/{id}/rows/{rowId}/history?before=: newest first.
func RowHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := tableID(w, r)
	if !ok {
		return
	}
	row, ok := idParam(r, "rowId")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid row id"})
		return
	}
	out, err := business.RowHistory(r.Context(), id, row, beforeParam(r), actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// Trash GET /tables/{id}/trash?before=: the table's deleted rows, newest
// first, and its deleted fields for someone who can change it.
func Trash(w http.ResponseWriter, r *http.Request) {
	id, ok := tableID(w, r)
	if !ok {
		return
	}
	out, err := business.TableTrash(r.Context(), id, beforeParam(r), actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// RestoreFromTrash POST /tables/{id}/trash/restore {rows, fields}.
func RestoreFromTrash(w http.ResponseWriter, r *http.Request) {
	id, ok := tableID(w, r)
	if !ok {
		return
	}
	var in business.RestoreInput
	if !decode(w, r, &in) {
		return
	}
	out, err := business.RestoreFromTrash(r.Context(), id, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// TablesTrash GET /tables/trash: the deleted tables the person may bring back.
func TablesTrash(w http.ResponseWriter, r *http.Request) {
	out, err := business.TablesTrash(r.Context(), actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// RestoreTable POST /tables/{id}/restore: a deleted table, back.
func RestoreTable(w http.ResponseWriter, r *http.Request) {
	id, ok := tableID(w, r)
	if !ok {
		return
	}
	t, err := business.RestoreTable(r.Context(), id, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}
