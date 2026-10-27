package controllers

import (
	"errors"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	importBusiness "github.com/akashc777/OneCamp/business/Import"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// What an admin does about an old import's access (business/Import
// recheck.go): re-check one that waits for an admin with their own connection
// to its tool, undo a re-check or a containment, or keep an import's tasks to
// its projects' admins until it is re-checked. Each answers at once and works
// in the background; the import's reviews say how it went. Admins only, in
// the app: never through a token, since agents and routines don't decide who
// sees what.

// importAccessAdmin is the admin acting on an old import's access: refused
// through a token, and to anyone who isn't an admin.
func importAccessAdmin(w http.ResponseWriter, r *http.Request) (userModel.UserInfo, bool) {
	if helpers.ActsThroughToken(r.Context()) {
		helpers.WriteForbidden(w, "Decide about an old import's access in the app.")
		return userModel.UserInfo{}, false
	}
	return httpx.RequireAdmin(w, r)
}

// RecheckImportAccess POST /admin/import-access/{import_id}/recheck: re-checks
// an old import that waits for an admin, or that an admin's containment
// keeps, with the admin's own connection to its tool, which must reach the
// import's own site.
func RecheckImportAccess(w http.ResponseWriter, r *http.Request) {
	a, ok := importAccessAdmin(w, r)
	if !ok {
		return
	}
	id, ok := helpers.UUIDParam(w, r, "import_id")
	if !ok {
		return
	}
	err := importBusiness.RecheckWithConnection(r.Context(), id, a.UserPostgresInfo.Id)
	if accessRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/RecheckImportAccess %s err: %+v", id, err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't start the re-check. Try again.")
		return
	}
	auditBusiness.Record(r, "import.access_recheck_started", auditBusiness.CategorySecurity,
		"Re-checked an old import against its source with their own connection", map[string]any{"import_id": id.String()})
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"msg": "Re-checking it against its source."})
}

// UndoImportAccessRecheck POST /admin/import-access/{import_id}/undo: puts
// back what an old import's re-check, or an admin's containment, changed in
// the last 30 days, where nobody has changed it since.
func UndoImportAccessRecheck(w http.ResponseWriter, r *http.Request) {
	a, ok := importAccessAdmin(w, r)
	if !ok {
		return
	}
	id, ok := helpers.UUIDParam(w, r, "import_id")
	if !ok {
		return
	}
	err := importBusiness.UndoRecheck(r.Context(), id, a.UserPostgresInfo.Id)
	if accessRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/UndoImportAccessRecheck %s err: %+v", id, err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't start putting it back. Try again.")
		return
	}
	auditBusiness.Record(r, "import.access_recheck_undone", auditBusiness.CategorySecurity,
		"Undid what an old import's re-check changed", map[string]any{"import_id": id.String()})
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"msg": "Putting it back as it was."})
}

// ContainImportAccess POST /admin/import-access/{import_id}/contain: keeps an
// old import's tasks that wait for an admin to their projects' admins until
// it is re-checked.
func ContainImportAccess(w http.ResponseWriter, r *http.Request) {
	a, ok := importAccessAdmin(w, r)
	if !ok {
		return
	}
	id, ok := helpers.UUIDParam(w, r, "import_id")
	if !ok {
		return
	}
	err := importBusiness.ContainImport(r.Context(), id, a.UserPostgresInfo.Id)
	if accessRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ContainImportAccess %s err: %+v", id, err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't start keeping its tasks. Try again.")
		return
	}
	auditBusiness.Record(r, "import.access_contained", auditBusiness.CategorySecurity,
		"Kept an old import's tasks to its projects' admins until it is re-checked", map[string]any{"import_id": id.String()})
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"msg": "Keeping its tasks to the projects' admins."})
}

// accessRefused answers an admin's action the import can't take now.
func accessRefused(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, importBusiness.ErrNotRecheckable):
		helpers.WriteError(w, http.StatusConflict, "That import has nothing waiting for this.")
	case errors.Is(err, importBusiness.ErrRecheckRunning):
		helpers.WriteError(w, http.StatusConflict, "That import is being re-checked. Try again once it's done.")
	case errors.Is(err, importBusiness.ErrNoConnection):
		helpers.WriteError(w, http.StatusConflict, "Connect the import's tool as yourself first.")
	case errors.Is(err, importBusiness.ErrOtherSource):
		helpers.WriteError(w, http.StatusConflict, "Your connection reaches a different site than the import's.")
	default:
		return false
	}
	return true
}
