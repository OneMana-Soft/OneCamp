package controllers

import (
	"net/http"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// RestoreMilestone brings a deleted milestone back to every project it showed
// in, with its tasks (POST /restore/milestone/{id}, controllers/Restore): its
// home project's admins, as its delete.
func RestoreMilestone(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	if _, _, ok := projectaccess.RequireID(w, r, owner.Project.String(), workAccess.NeedManage, adminsOnly); !ok {
		return
	}
	if err := softDelete.RestoreAs(r.Context(), softDelete.Milestone, id, nil); err != nil {
		write(w, r, "RestoreMilestone", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Milestone restored."})
}
