package controller

import (
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/TaskField"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// RestoreField brings a deleted field back with every task's value of it
// (POST /restore/field/{id}, controllers/Restore): its project's admins, as
// its delete.
func RestoreField(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	if _, _, ok := projectaccess.RequireID(w, r, owner.Project.String(), workAccess.NeedManage, adminOnly); !ok {
		return
	}
	if err := business.Restore(r.Context(), id); err != nil {
		if errors.Is(err, business.ErrNameTaken) {
			// Its own words: create's give no way back.
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "This project already has a field with that name. Rename that one, then restore this field."})
			return
		}
		fail(w, r, "RestoreField", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Field restored."})
}
