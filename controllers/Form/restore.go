package controllers

import (
	"net/http"

	formBusiness "github.com/akashc777/OneCamp/business/Form"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// RestoreForm brings a deleted form back: its link takes answers again, and
// its submissions and what they made are its own again (POST
// /restore/form/{id}, controllers/Restore). A project's form: its project's
// admins; a table's form: whoever manages the table. Each as its delete.
func RestoreForm(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	if owner.Project != uuid.Nil {
		if _, _, ok := projectaccess.RequireID(w, r, owner.Project.String(), workAccess.NeedManage, denied); !ok {
			return
		}
	} else {
		userInfo, signedIn := httpx.MustUser(w, r)
		if !signedIn {
			return
		}
		if err := formBusiness.MayManageTable(r.Context(), owner.Table, userInfo.UserPostgresInfo.Id); err != nil {
			writeTable(w, r, "RestoreForm", err)
			return
		}
	}
	if err := softDelete.RestoreAs(r.Context(), softDelete.Form, id, nil); err != nil {
		write(w, r, "RestoreForm", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Form restored."})
}
