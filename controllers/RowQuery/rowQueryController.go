// Package controllers (RowQuery) is POST /tables/{id}/query (tables A3): a
// window of a table's rows for a spec (business/QuerySpec's shape, its scope
// set to the table), answered on the server over every row the reader may see.
package controllers

import (
	"encoding/json"
	"net/http"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	tableController "github.com/akashc777/OneCamp/controllers/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// maxBody is the most a query's body may be.
const maxBody = 64 << 10

// request is a query: its spec, which window, and whether a first window
// counts its set (a reader asks for that in a request of its own, beside its
// first window, so the rows never wait for the count).
type request struct {
	Spec   query.Spec   `json:"spec"`
	Cursor query.Cursor `json:"cursor,omitempty"`
	N      int          `json:"n,omitempty"`
	Count  bool         `json:"count,omitempty"`
}

// QueryRows POST /tables/{id}/query {spec, cursor, n, count} → {rows, next, total, total_capped, summaries}.
func QueryRows(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	out, err := dataTable.QueryRows(tableController.ReaderCtx(r), id, tableController.ActorFrom(r), req.Spec, query.Page{Size: req.N, After: req.Cursor}, req.Count)
	if err != nil {
		tableController.WriteQueryErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}
