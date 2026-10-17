// Package controllers (Follow) answers /follows: how much someone hears about
// one thing (business/Follow), and /admin/policies, the workspace's policies
// (business/Policy).
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Follow"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// maxBody bounds what a follow or a policy change may send.
const maxBody = 64 << 10

// followInput is a choice: the thing, and the mode.
type followInput struct {
	ResourceKind string `json:"resource_kind"`
	ResourceUUID string `json:"resource_uuid"`
	Mode         string `json:"mode"`
}

// subject is the signed-in person, from server context (I11).
func subject(w http.ResponseWriter, r *http.Request) (accessBusiness.Subject, bool) {
	_, who := accessBusiness.RequestSubject(r)
	if who.UserUID == "" {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
		return accessBusiness.Subject{}, false
	}
	return who, true
}

func writeFollow(w http.ResponseWriter, r *http.Request, st business.State, err error) {
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": st})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such page, or you can't open it."})
	case errors.Is(err, business.ErrUnknownKind):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That kind of thing can't be followed yet."})
	case errors.Is(err, business.ErrBadMode):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Choose all, replies and mentions, or none."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Follow err: %+v", err)
		helpers.WriteRetryLater(w, "That couldn't be changed just now.")
	}
}

// GetFollow answers the signed-in person's follow on one thing:
// GET /follows?resource_kind=doc&resource_uuid=….
func GetFollow(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	st, err := business.Get(r.Context(), who, q.Get("resource_kind"), q.Get("resource_uuid"))
	writeFollow(w, r, st, err)
}

// SetFollow records their choice: PUT /follows {resource_kind, resource_uuid, mode}.
func SetFollow(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	var in followInput
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That choice couldn't be read."})
		return
	}
	st, err := business.Set(r.Context(), who, in.ResourceKind, in.ResourceUUID, in.Mode)
	writeFollow(w, r, st, err)
}

// ClearFollow forgets it, so their default applies again:
// DELETE /follows?resource_kind=doc&resource_uuid=….
func ClearFollow(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	st, err := business.Clear(r.Context(), who, q.Get("resource_kind"), q.Get("resource_uuid"))
	writeFollow(w, r, st, err)
}

// ListPolicies answers every workspace policy, with its value and default:
// GET /admin/policies.
func ListPolicies(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": policyBusiness.Admin.ListAll(r.Context())})
}

// SetPolicy changes one, and records the change:
// PUT /admin/policies/{key} {"value": …}.
func SetPolicy(w http.ResponseWriter, r *http.Request) {
	u, ok := userModels.FromContext(r.Context())
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
		return
	}
	var in struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil || len(in.Value) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That value couldn't be read."})
		return
	}
	info, err := policyBusiness.Admin.Set(r.Context(), chi.URLParam(r, "key"), in.Value,
		policyBusiness.Changer{UserID: u.UserPostgresInfo.Id, Email: u.UserPostgresInfo.EmailID})
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": info})
	case errors.Is(err, policyBusiness.ErrUnknownPolicy):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such policy."})
	case errors.Is(err, policyBusiness.ErrBadValue):
		why := strings.TrimPrefix(err.Error(), policyBusiness.ErrBadValue.Error()+": ")
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That value doesn't fit: " + why + "."})
	case errors.Is(err, policyBusiness.ErrNotRecorded):
		helpers.WriteRetryLater(w, "The change couldn't be recorded in the audit log, so it wasn't made.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/SetPolicy err: %+v", err)
		helpers.WriteRetryLater(w, "That couldn't be changed just now.")
	}
}
