package controllers

import (
	"net/http"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
)

const seatUpgradeURL = helpers.PlanUpgradeURL

// GetSeats handles GET /admin/seats: how many people the workspace has and
// how many its licence covers (limit 0 = unlimited), so the admin sees a
// full free plan coming instead of meeting it at someone's sign-up.
func GetSeats(w http.ResponseWriter, r *http.Request) {
	used, limit, err := userDomain.SeatUsage(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetSeats err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not count members", "status": "failed"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data": map[string]interface{}{
			"used": used, "limit": limit,
			// Where the limit is removed. From the server, so the web app names
			// no deployment of its own.
			"upgrade_url": seatUpgradeURL,
			// The plan beside its seat limit: what the free plan leaves out, so
			// admin screens can explain a locked control before anyone clicks it.
			"free_plan": helpers.OnFreePlan(),
			"locked":    helpers.LockedFeatures(),
		},
	})
}
