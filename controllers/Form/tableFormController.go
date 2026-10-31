package controllers

// A table's own forms (tables B5), for the table's managers: the business
// layer decides who may (business/Form/tableForms.go).

import (
	"errors"
	"net/http"

	formBusiness "github.com/akashc777/OneCamp/business/Form"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// tableOf is the request's table and the person asking, or false once it has
// answered.
func tableOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	table, ok := helpers.UUIDParam(w, r, "id")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return table, u.UserPostgresInfo.Id, true
}

// tableFormOf is the request's table, its form and the person asking, or
// false once it has answered.
func tableFormOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	table, by, ok := tableOf(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	form, ok := helpers.UUIDParam(w, r, "form_id")
	return table, form, by, ok
}

// writeTable answers a table form's error: 404 for a table that isn't there,
// 403 for one the person can't manage, else as write does.
func writeTable(w http.ResponseWriter, r *http.Request, where string, err error) {
	switch {
	case errors.Is(err, formBusiness.ErrTableNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Table not found"})
	case errors.Is(err, formBusiness.ErrTableForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the table's owner or an admin can change its forms."})
	default:
		write(w, r, where, err)
	}
}

// ListTableForms is GET /tables/{id}/forms.
func ListTableForms(w http.ResponseWriter, r *http.Request) {
	table, by, ok := tableOf(w, r)
	if !ok {
		return
	}
	forms, err := formBusiness.TableForms(r.Context(), table, by)
	if err != nil {
		writeTable(w, r, "ListTableForms", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"forms": forms, "can_edit": true}})
}

// SaveTableForm is POST /tables/{id}/forms: a new form, or a change to one.
func SaveTableForm(w http.ResponseWriter, r *http.Request) {
	table, by, ok := tableOf(w, r)
	if !ok {
		return
	}
	var in formBusiness.Input
	if !httpx.ReadJSON(w, r, inputLimit, &in) {
		return
	}
	f, err := formBusiness.SaveTableForm(r.Context(), table, in, by)
	if err != nil {
		writeTable(w, r, "SaveTableForm", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": f})
}

// DeleteTableForm is POST /tables/{id}/forms/{form_id}/delete.
func DeleteTableForm(w http.ResponseWriter, r *http.Request) {
	table, formID, by, ok := tableFormOf(w, r)
	if !ok {
		return
	}
	deleted, err := formBusiness.DeleteTableForm(r.Context(), table, formID, by)
	if err != nil {
		writeTable(w, r, "DeleteTableForm", err)
		return
	}
	writeDeleted(w, deleted)
}

// TableFormSubmissions is GET /tables/{id}/forms/{form_id}/submissions: the
// latest answers, each with the row it added.
func TableFormSubmissions(w http.ResponseWriter, r *http.Request) {
	table, formID, by, ok := tableFormOf(w, r)
	if !ok {
		return
	}
	f, err := formBusiness.TableForm(r.Context(), table, formID, by)
	if err != nil {
		writeTable(w, r, "TableFormSubmissions", err)
		return
	}
	writeSubmissions(w, r, "TableFormSubmissions", f)
}
