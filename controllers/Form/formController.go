package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	formBusiness "github.com/akashc777/OneCamp/business/Form"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Intake forms: see business/Form. A project's members see its forms; its
// admins make and change them; anyone with the link fills one in.

const denied = "Only the project's admins can change its forms."

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var fe *formBusiness.FormError
	switch {
	case errors.As(err, &fe):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": fe.Error()})
	case errors.Is(err, formBusiness.ErrNoForm):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "This form isn't available."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Form/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Something went wrong. Try again in a moment."})
	}
}

// ListForms. GET /project/{p}/forms
func ListForms(w http.ResponseWriter, r *http.Request) {
	id, p, ok := projectaccess.Require(w, r, false, denied)
	if !ok {
		return
	}
	forms, err := formModel.List(id)
	if err != nil {
		write(w, r, "ListForms", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"forms": forms, "can_edit": p.IsProjectAdmin > 0}})
}

// SaveForm creates or updates a form. POST /project/{p}/forms
func SaveForm(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, true, denied)
	if !ok {
		return
	}
	var in formBusiness.Input
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	f, err := formBusiness.Save(id, in, userInfo.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "SaveForm", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": f})
}

// DeleteForm; tasks it made stay. POST /project/{p}/forms/{form_id}/delete
func DeleteForm(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, true, denied)
	if !ok {
		return
	}
	formID, err := uuid.Parse(chi.URLParam(r, "form_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Form not found"})
		return
	}
	deleted, err := formModel.Delete(id, formID)
	if err != nil {
		write(w, r, "DeleteForm", err)
		return
	}
	if !deleted {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Form not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// GetPublicForm is a form's questions, for anyone with the link.
// GET /public/form/{token}
func GetPublicForm(w http.ResponseWriter, r *http.Request) {
	f, err := formBusiness.GetPublic(chi.URLParam(r, "token"))
	if err != nil {
		write(w, r, "GetPublicForm", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": f})
}

// SubmitPublicForm turns answers into a task. POST /public/form/{token}
func SubmitPublicForm(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Answers map[string]any `json:"answers"`
		// Website is a field people never see: a form that fills it is a bot.
		Website string `json:"website"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	if in.Website != "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't send that."})
		return
	}
	if err := formBusiness.Submit(r.Context(), chi.URLParam(r, "token"), in.Answers, time.Now()); err != nil {
		write(w, r, "SubmitPublicForm", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Sent"})
}
