package controller

import (
	"net/http"

	business "github.com/akashc777/OneCamp/business/Invoice"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// RestoreInvoice brings a deleted draft back with its number, which no other
// invoice took meanwhile (POST /restore/invoice/{id}, controllers/Restore):
// its project's admins, as its delete.
func RestoreInvoice(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	if _, _, ok := projectaccess.RequireID(w, r, owner.Project.String(), workAccess.NeedManage, adminOnly); !ok {
		return
	}
	if err := business.Restore(r.Context(), id); err != nil {
		fail(w, r, "RestoreInvoice", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Invoice restored."})
}
