package controller

import (
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// Restore brings a deleted status back with the tasks its delete moved, each
// still where the delete put it (POST /restore/status/{id},
// controllers/Restore): its project's admins, as its delete.
func Restore(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	projectID, _, ok := accessTo(w, r, owner.Project, admin)
	if !ok {
		return
	}
	s, err := business.Restore(r.Context(), id)
	if errors.Is(err, business.ErrUnfinished) {
		// It is live again, so open views show it; the next restore (the
		// Undo's retry) finishes the rest.
		changed(projectID)
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskStatus Restore %s err: %+v", id, err)
		helpers.WriteRetryLater(w, "The status is back, but its restore didn't finish.")
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	changed(projectID)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}
