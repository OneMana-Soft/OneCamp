package controller

import (
	"errors"
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// RestoreTime brings back one of the caller's deleted time entries, into
// reports and billing again (POST /restore/time_entry/{id},
// controllers/Restore): only its own person, as its delete.
func RestoreTime(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	if owner.Person != me(r).UserPostgresInfo.Id {
		helpers.WriteNotFound(w, "That entry isn't yours to restore.")
		return
	}
	err := softDelete.RestoreAs(r.Context(), softDelete.TimeEntry, id, nil)
	if errors.Is(err, softDelete.ErrTaken) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Another timer of yours is running. Stop it, then restore this entry."})
		return
	}
	if err != nil {
		fail(w, r, "RestoreTime", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Time entry restored."})
}
