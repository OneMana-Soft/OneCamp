// Package controllers (Departed) is the admin's view of work under the name
// of people who have left, and of how many links a shorter link lifetime
// would stop (docs C6-2). Admin routes only; every change is in the audit log.
package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	departedBusiness "github.com/akashc777/OneCamp/business/Departed"
	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Seams for tests.
var (
	list         = departedBusiness.List
	handOver     = departedBusiness.HandOver
	turnOff      = departedBusiness.TurnOffLinks
	stoppedByCap = guestModel.CountStoppedByCap
)

// maxCapDays is the longest link lifetime an admin can set (business/Policy).
const maxCapDays = 3650

// ListDeparted GET /admin/departed: the people who left with docs or links
// still under their name.
func ListDeparted(w http.ResponseWriter, r *http.Request) {
	people, err := list(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Departed/ListDeparted err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "The people who left couldn't be read. Try again in a moment."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"people": people}})
}

// HandOver POST /admin/departed/{user_uuid}/handover {to_user_uuid}: every
// doc and link under a departed person's name, to someone in the workspace.
func HandOver(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	from, ok := personOf(w, r)
	if !ok {
		return
	}
	var in struct {
		To string `json:"to_user_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(strings.ToLower(in.To)) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say who to hand their work to."})
		return
	}
	to := uuid.MustParse(strings.ToLower(in.To))
	moved, err := handOver(ctx, who, from, to)
	// Whatever moved is recorded, even when the rest couldn't be.
	if moved.Docs > 0 || moved.Links > 0 {
		adminAuditBusiness.Record(r, "doc.handover_departed", adminAuditBusiness.CategorySecurity,
			"Handed on the work of someone who left", map[string]interface{}{
				"from_user_uuid": from.String(), "to_user_uuid": to.String(), "docs": moved.Docs, "links": moved.Links, "failed": moved.Failed,
			})
	}
	if err != nil {
		if !refused(w, err) {
			helpers.LogErrorWithContext(ctx, "controllers/Departed/HandOver err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Their work couldn't all be handed on. Try again: what moved stays moved."})
		}
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": moved})
}

// TurnOffLinks POST /admin/departed/{user_uuid}/links/off: every live link
// and page a departed person made or published, revoked.
func TurnOffLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	from, ok := personOf(w, r)
	if !ok {
		return
	}
	n, err := turnOff(ctx, who, from)
	if err != nil {
		if !refused(w, err) {
			helpers.LogErrorWithContext(ctx, "controllers/Departed/TurnOffLinks err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Their links couldn't be turned off. Try again in a moment."})
		}
		return
	}
	adminAuditBusiness.Record(r, "guest.links.departed_off", adminAuditBusiness.CategorySecurity,
		"Turned off the links of someone who left", map[string]interface{}{"user_uuid": from.String(), "links": n})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]int{"links": n}})
}

// CountStoppedLinks GET /admin/guest-links/stopped?days=N: how many live links a
// longest lifetime of N days would stop now, for the admin's confirmation.
func CountStoppedLinks(w http.ResponseWriter, r *http.Request) {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days < 0 || days > maxCapDays {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say how many days, from 0 to 3650."})
		return
	}
	n, err := stoppedByCap(r.Context(), days)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "The links couldn't be counted. Try again in a moment."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]int{"days": days, "links": n}})
}

// personOf is the {user_uuid} a route names.
func personOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id := strings.ToLower(chi.URLParam(r, "user_uuid"))
	if !helpers.IsCanonicalUUID(id) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say whose work this is."})
		return uuid.Nil, false
	}
	return uuid.MustParse(id), true
}

// refused answers a refusal the business said in words, and reports whether
// it did.
func refused(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, departedBusiness.ErrNotAdmin):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only a workspace admin can do that."})
	case errors.Is(err, departedBusiness.ErrNotDeparted):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That person hasn't left the workspace."})
	case errors.Is(err, departedBusiness.ErrNotAMember):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Work can be handed only to someone in this workspace."})
	default:
		return false
	}
	return true
}
