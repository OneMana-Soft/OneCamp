package controllers

import (
	"net/http"

	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// RestoreCycle brings a deleted cycle back with its tasks (POST
// /restore/cycle/{id}, controllers/Restore): its project's admins, as its
// delete.
func RestoreCycle(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	if _, _, ok := projectaccess.RequireID(w, r, owner.Project.String(), workAccess.NeedManage, adminsOnly); !ok {
		return
	}
	if err := cycleBusiness.Restore(r.Context(), id); err != nil {
		write(w, r, "RestoreCycle", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Cycle restored."})
}
