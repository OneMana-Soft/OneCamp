package controllers

// A row's page over HTTP (tables T2, rows as pages; business/DataTable
// rowPages.go): reading it with the row's record, and making it the first
// time someone writes in it. The row decides who may do either, by its own
// table: a row that isn't in the table named, or isn't the person's to open,
// answers as a row that isn't there.

import (
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// pageTarget is the table and row a page route names, and who asks.
func pageTarget(w http.ResponseWriter, r *http.Request) (accessBusiness.Subject, uuid.UUID, uuid.UUID, bool) {
	table, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return accessBusiness.Subject{}, uuid.Nil, uuid.Nil, false
	}
	row, ok := idParam(r, "rowId")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid row id"})
		return accessBusiness.Subject{}, uuid.Nil, uuid.Nil, false
	}
	user, _ := userModels.FromContext(r.Context())
	return accessBusiness.SubjectFor(r.Context(), user), table, row, true
}

// writePageErr answers a page route's refusal or failure.
func writePageErr(w http.ResponseWriter, r *http.Request, err error, where string) {
	switch {
	case business.IsRowNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "row not found"})
	case business.IsPageReadOnly(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": err.Error()})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "Couldn't open the row's page just now.")
	}
}

// GetRowPage GET /tables/{id}/rows/{rowId}/page: the row's page, for someone
// who may open the row: {doc_uuid, body, level}, with no doc_uuid while the
// row has none.
func GetRowPage(w http.ResponseWriter, r *http.Request) {
	who, table, row, ok := pageTarget(w, r)
	if !ok {
		return
	}
	page, err := business.RowPageFor(r.Context(), who, table, row)
	if err != nil {
		writePageErr(w, r, err, "GetRowPage")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
}

// EnsureRowPage POST /tables/{id}/rows/{rowId}/page: the row's page, made now
// when it has none, for someone who may edit the row: {doc_uuid, made}.
func EnsureRowPage(w http.ResponseWriter, r *http.Request) {
	who, table, row, ok := pageTarget(w, r)
	if !ok {
		return
	}
	doc, made, err := business.EnsureRowPageFor(r.Context(), who, table, row)
	if err != nil {
		writePageErr(w, r, err, "EnsureRowPage")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"doc_uuid": doc, "made": made}})
}
