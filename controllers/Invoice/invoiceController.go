// Package controller (Invoice) serves a project's saved invoices. Money is
// its admins' only, as its rates are. See business/Invoice.
package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Invoice"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const adminOnly = "Only the project's admins see and make its invoices."

func fail(w http.ResponseWriter, r *http.Request, where string, err error) {
	var ie *business.InputError
	switch {
	case errors.As(err, &ie):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ie.Error()})
	case errors.Is(err, business.ErrNumberTaken):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "An invoice with that number already exists. Give this one another."})
	case errors.Is(err, business.ErrNotDraft):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "This invoice has been sent, so it stays as it was sent. Void it and make a new one to change it."})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That invoice no longer exists."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Invoice/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "That couldn't be saved just now. Try again in a moment."})
	}
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 512<<10)).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That invoice couldn't be read."})
		return false
	}
	return true
}

func invoiceID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "invoice_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That invoice no longer exists."})
		return uuid.Nil, false
	}
	return id, true
}

// ListInvoices is a project's invoices, and the number the next would take.
// GET /project/{p}/invoices
func ListInvoices(w http.ResponseWriter, r *http.Request) {
	id, p, ok := projectaccess.Require(w, r, true, adminOnly)
	if !ok {
		return
	}
	invoices, next, err := business.List(r.Context(), id, p.Name)
	if err != nil {
		fail(w, r, "ListInvoices", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"invoices": invoices, "next_number": next}})
}

// GetInvoice is one invoice. GET /project/{p}/invoices/{i}
func GetInvoice(w http.ResponseWriter, r *http.Request) {
	project, _, ok := projectaccess.Require(w, r, true, adminOnly)
	if !ok {
		return
	}
	id, ok := invoiceID(w, r)
	if !ok {
		return
	}
	inv, err := business.Get(r.Context(), project, id)
	if err != nil {
		fail(w, r, "GetInvoice", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": inv})
}

// CreateInvoice saves an invoice. POST /project/{p}/invoices
func CreateInvoice(w http.ResponseWriter, r *http.Request) {
	project, _, ok := projectaccess.Require(w, r, true, adminOnly)
	if !ok {
		return
	}
	var in business.Input
	if !decode(w, r, &in) {
		return
	}
	user := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	inv, err := business.Create(r.Context(), project, in, user.UserPostgresInfo.Id)
	if err != nil {
		fail(w, r, "CreateInvoice", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": inv})
}

// UpdateInvoice changes a draft. POST /project/{p}/invoices/{i}
func UpdateInvoice(w http.ResponseWriter, r *http.Request) {
	project, _, ok := projectaccess.Require(w, r, true, adminOnly)
	if !ok {
		return
	}
	id, ok := invoiceID(w, r)
	if !ok {
		return
	}
	var in business.Input
	if !decode(w, r, &in) {
		return
	}
	inv, err := business.Update(r.Context(), project, id, in)
	if err != nil {
		fail(w, r, "UpdateInvoice", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": inv})
}

// SetInvoiceStatus marks an invoice sent, paid, back to a draft, or void.
// POST /project/{p}/invoices/{i}/status {"status": "paid"}
func SetInvoiceStatus(w http.ResponseWriter, r *http.Request) {
	project, _, ok := projectaccess.Require(w, r, true, adminOnly)
	if !ok {
		return
	}
	id, ok := invoiceID(w, r)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &body) {
		return
	}
	inv, err := business.SetStatus(r.Context(), project, id, body.Status)
	if err != nil {
		fail(w, r, "SetInvoiceStatus", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": inv})
}

// DeleteInvoice removes a draft. POST /project/{p}/invoices/{i}/delete
func DeleteInvoice(w http.ResponseWriter, r *http.Request) {
	project, _, ok := projectaccess.Require(w, r, true, adminOnly)
	if !ok {
		return
	}
	id, ok := invoiceID(w, r)
	if !ok {
		return
	}
	if err := business.Delete(r.Context(), project, id); err != nil {
		fail(w, r, "DeleteInvoice", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}
