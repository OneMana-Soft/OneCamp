package controllers

import (
	"net/http"

	planBusiness "github.com/akashc777/OneCamp/business/Plan"
	"github.com/akashc777/OneCamp/helpers"
)

// GetPlan handles GET /admin/plan, and GET /admin/seats for older web builds:
// what this workspace is on, its seats against its limit (0 = unlimited),
// which company controls it includes, the running release, and the ways past
// the free plan, so every admin screen reads one answer and names no host.
func GetPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := planBusiness.Current(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetPlan err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not count members", "status": "failed"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": plan})
}

// GetPlanPrices handles GET /plan/prices, for an admin or the demo's visitor:
// the storefront's prices, asked for only now, or {"available": false} when
// this server can't reach it (the page then links the pricing page).
func GetPlanPrices(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": planBusiness.CurrentPrices(r.Context())})
}
