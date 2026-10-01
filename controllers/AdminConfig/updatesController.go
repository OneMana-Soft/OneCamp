package controllers

import (
	"net/http"

	updatesBusiness "github.com/akashc777/OneCamp/business/Updates"
	"github.com/akashc777/OneCamp/helpers"
)

// CheckForUpdates handles GET /admin/updates: this workspace's release against
// the current one on its edition. Admin-only, and it reaches onemana.dev only
// because an admin asked.
func CheckForUpdates(w http.ResponseWriter, r *http.Request) {
	status, err := updatesBusiness.Check(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/CheckForUpdates err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadGateway, helpers.Envolope{
			"msg":    "Couldn't reach backend.onemana.dev to check. Make sure this server can reach it, then try again.",
			"status": "failed",
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": status})
}
