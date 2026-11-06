package controllers

// Sensitivity labels over HTTP (miro-44 head L, business/Label). Admins define
// labels under /admin (router/labels.go); members list them, read an item's,
// and put one on an item they may edit. Every change is a row in the audit log
// before it is answered, or it isn't made. Guests never reach these routes.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	labelBusiness "github.com/akashc777/OneCamp/business/Label"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// maxBody bounds a request's JSON.
const maxBody = 16 << 10

// who is the signed-in person acting, or a 401 answered. An agent acting for
// them never acts as an admin (Subject.ActingAdmin).
func who(w http.ResponseWriter, r *http.Request) (labelBusiness.Who, bool) {
	u, s := accessBusiness.RequestSubject(r)
	if s.UserUUID == "" || u.UserPostgresInfo.Id == uuid.Nil {
		helpers.WriteUnauthorized(w)
		return labelBusiness.Who{}, false
	}
	return labelBusiness.Who{UserUID: s.UserUID, UserUUID: s.UserUUID, UserID: u.UserPostgresInfo.Id, Admin: s.ActingAdmin(), AgentUUID: s.AgentUUID}, true
}

// fail answers a business error as its status, and logs the unexpected ones.
func fail(w http.ResponseWriter, r *http.Request, err error, what string) {
	switch {
	case helpers.WriteRefusal(w, err):
	case errors.Is(err, labelBusiness.ErrUnknownKind):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Only boards and docs carry labels."})
	case errors.Is(err, labelBusiness.ErrItemNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That isn't here, or you can't open it."})
	case errors.Is(err, labelBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such label. It may have been removed."})
	case errors.Is(err, labelBusiness.ErrNameTaken):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "Another label has that name. Choose another."})
	case errors.Is(err, labelBusiness.ErrAdminOnly):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only a workspace admin can add, change or remove labels."})
	case errors.Is(err, labelBusiness.ErrMayNotLabel):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You need edit access to label this. Ask its owner."})
	case errors.Is(err, labelBusiness.ErrBadRequest):
		why := strings.TrimPrefix(err.Error(), labelBusiness.ErrBadRequest.Error()+": ")
		if why != "" {
			why = strings.ToUpper(why[:1]) + why[1:] + "."
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": why})
	case errors.Is(err, auditBusiness.ErrNotRecorded):
		helpers.WriteRetryLater(w, "That couldn't be recorded in the audit log, so it wasn't done.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Label %s: %+v", what, err)
		helpers.WriteRetryLater(w, "That couldn't be done just now.")
	}
}

// ListLabels handles GET /labels: every live label, and the rules a label can
// carry, for the picker and the admin card.
func ListLabels(w http.ResponseWriter, r *http.Request) {
	if _, ok := who(w, r); !ok {
		return
	}
	ls, err := labelBusiness.Labels(r.Context())
	if err != nil {
		fail(w, r, err, "ListLabels")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"labels": ls, "rules": labelBusiness.Rules()}})
}

// input reads a label's fields.
func input(w http.ResponseWriter, r *http.Request) (labelBusiness.Input, bool) {
	var in labelBusiness.Input
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That label couldn't be read."})
		return in, false
	}
	return in, true
}

// labelID is the {id} in the path, or a 404 answered.
func labelID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such label. It may have been removed."})
		return uuid.Nil, false
	}
	return id, true
}

// DefineLabel handles POST /admin/labels {"name", "colour", "rank", "rules"}.
func DefineLabel(w http.ResponseWriter, r *http.Request) {
	p, ok := who(w, r)
	if !ok {
		return
	}
	in, ok := input(w, r)
	if !ok {
		return
	}
	l, err := labelBusiness.Define(r.Context(), p, in)
	if err != nil {
		fail(w, r, err, "DefineLabel")
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"msg": "ok", "data": l})
}

// ChangeLabel handles PATCH /admin/labels/{id} with any of DefineLabel's fields.
func ChangeLabel(w http.ResponseWriter, r *http.Request) {
	p, ok := who(w, r)
	if !ok {
		return
	}
	id, ok := labelID(w, r)
	if !ok {
		return
	}
	in, ok := input(w, r)
	if !ok {
		return
	}
	l, err := labelBusiness.Change(r.Context(), p, id, in)
	if err != nil {
		fail(w, r, err, "ChangeLabel")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": l})
}

// RemoveLabel handles POST /admin/labels/{id}/remove: its rules stop holding
// on every item it is on. The label's row stays, stamped removed.
func RemoveLabel(w http.ResponseWriter, r *http.Request) {
	p, ok := who(w, r)
	if !ok {
		return
	}
	id, ok := labelID(w, r)
	if !ok {
		return
	}
	if err := labelBusiness.Remove(r.Context(), p, id); err != nil {
		fail(w, r, err, "RemoveLabel")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok"})
}

// ItemLabel handles GET /labels/items/{kind}/{id}: the label on an item the
// person may read ({"data": null} for none).
func ItemLabel(w http.ResponseWriter, r *http.Request) {
	p, ok := who(w, r)
	if !ok {
		return
	}
	l, err := labelBusiness.OfItem(r.Context(), p, chi.URLParam(r, "kind"), chi.URLParam(r, "id"))
	if err != nil {
		fail(w, r, err, "ItemLabel")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": l})
}

// ItemLabels handles GET /labels/items/{kind}?ids=a,b: the label on each of
// these items that the person may read and that carries one, by id (a list's
// chips). At most 100 ids at a time.
func ItemLabels(w http.ResponseWriter, r *http.Request) {
	p, ok := who(w, r)
	if !ok {
		return
	}
	var ids []string
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	out, err := labelBusiness.OfItems(r.Context(), p, chi.URLParam(r, "kind"), ids)
	if err != nil {
		fail(w, r, err, "ItemLabels")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// PutItemLabel handles PUT /labels/items/{kind}/{id} {"label_id": "<id>" or
// null}: puts a label on an item, or takes its label off, and answers the
// label it carries after.
func PutItemLabel(w http.ResponseWriter, r *http.Request) {
	p, ok := who(w, r)
	if !ok {
		return
	}
	var body struct {
		LabelID *string `json:"label_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the label's id, or null to take the label off."})
		return
	}
	var lid *uuid.UUID
	if body.LabelID != nil {
		id, err := uuid.Parse(*body.LabelID)
		if err != nil {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such label. It may have been removed."})
			return
		}
		lid = &id
	}
	l, err := labelBusiness.Put(r.Context(), p, chi.URLParam(r, "kind"), chi.URLParam(r, "id"), lid)
	if err != nil {
		fail(w, r, err, "PutItemLabel")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": l})
}
