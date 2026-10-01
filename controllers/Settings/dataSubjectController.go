package controllers

// Admin: where does one person's data live?
//
// The first thing either a subject access request or an erasure request needs is
// an answer to that question, and until now it could only be answered by reading
// the schema by hand. Read-only: it counts rows, it never deletes.

import (
	"net/http"

	dataSubjectBusiness "github.com/akashc777/OneCamp/business/DataSubject"
	"github.com/akashc777/OneCamp/helpers"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// GetPersonalDataInventory handles GET /admin/data-subject/{userId}/inventory
func GetPersonalDataInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid user id"})
		return
	}

	locations, err := dataSubjectBusiness.Inventory(ctx, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetPersonalDataInventory err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to build the inventory"})
		return
	}
	if locations == nil {
		locations = []dataSubjectBusiness.PersonalDataLocation{}
	}

	helpers.LogInfoWithContext(ctx, "audit: personal-data inventory read for user %s", userID)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"user_id":    userID,
		"locations":  locations,
		"total_rows": dataSubjectBusiness.TotalRows(locations),
		// Stated in the payload, not only the docs. An operator who reads this
		// response should not mistake counts for an Article 15 answer, which
		// requires a copy of the data itself.
		"note": "Counts only. A subject access request requires a copy of the personal data, not a list of locations.",
	}})
}

// GetCSPViolations handles GET /admin/csp-violations
//
// The evidence for deciding whether the Content-Security-Policy is safe to
// enforce. Aggregated, so each row is one distinct thing that would break rather
// than one page load that already happened.
func GetCSPViolations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	items, err := configModels.ListCSPViolations(ctx, 200)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetCSPViolations err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Could not load the violation list",
		})
		return
	}
	if items == nil {
		items = []*configModels.CSPViolation{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// ClearCSPViolations handles POST /admin/csp-violations/clear
//
// Needed because the point of a validation window is to fix something and see
// whether it stops. Without a reset an operator cannot tell a violation they
// just fixed from one still happening: both keep an old first_seen and a count
// that only grows.
func ClearCSPViolations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := configModels.ClearCSPViolations(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ClearCSPViolations err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Could not clear the list",
		})
		return
	}
	helpers.LogInfoWithContext(ctx, "audit: CSP violation list cleared")
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "cleared"})
}
