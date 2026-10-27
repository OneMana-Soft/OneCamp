package controllers

import (
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	importBusiness "github.com/akashc777/OneCamp/business/Import"
	"github.com/akashc777/OneCamp/helpers"
)

// ListImportAccess GET /admin/import-access: the admins' card of old imports'
// access re-check, each import with its projects, where it stands and its
// counts, naming tasks by id only. Admins only, in the app.
func ListImportAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := importAccessAdmin(w, r); !ok {
		return
	}
	imports, err := importBusiness.ImportAccessList(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ListImportAccess err: %+v", err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't load the old imports' re-check. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"imports": imports})
}

// KeepImportAccess POST /admin/import-access/{import_id}/keep: keeps an old
// import that waits for an admin as it is. Admins only, in the app.
func KeepImportAccess(w http.ResponseWriter, r *http.Request) {
	a, ok := importAccessAdmin(w, r)
	if !ok {
		return
	}
	id, ok := helpers.UUIDParam(w, r, "import_id")
	if !ok {
		return
	}
	err := importBusiness.KeepImportAccess(r.Context(), id, a.UserPostgresInfo.Id)
	if accessRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/KeepImportAccess %s err: %+v", id, err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't keep it as it is. Try again.")
		return
	}
	auditBusiness.Record(r, "import.access_kept", auditBusiness.CategorySecurity,
		"Kept an old import's access as it is", map[string]any{"import_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Kept as it is."})
}
