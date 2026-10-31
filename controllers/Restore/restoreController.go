// Package controllers (Restore) is the one route that brings back what a soft
// delete kept (V-78): POST /restore/{kind}/{id}, the web's Undo right after a
// delete, and a restore later. The kind names the table
// (models/postgres/SoftDelete); each kind's own controller checks the caller
// as its delete does, restores, and answers in its own words.
package controllers

import (
	"net/http"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	formController "github.com/akashc777/OneCamp/controllers/Form"
	invoiceController "github.com/akashc777/OneCamp/controllers/Invoice"
	milestoneController "github.com/akashc777/OneCamp/controllers/Milestone"
	taskFieldController "github.com/akashc777/OneCamp/controllers/TaskField"
	taskStatusController "github.com/akashc777/OneCamp/controllers/TaskStatus"
	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// kind is a kind's restore: the caller's check, the restore and the answer,
// given the deleted row's id and whom it belongs to.
type kind func(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner)

var kinds = map[softDelete.Kind]kind{
	softDelete.Field:     taskFieldController.RestoreField,
	softDelete.Status:    taskStatusController.Restore,
	softDelete.Cycle:     cycleController.RestoreCycle,
	softDelete.Milestone: milestoneController.RestoreMilestone,
	softDelete.Form:      formController.RestoreForm,
	softDelete.TimeEntry: timeEntryController.RestoreTime,
	softDelete.Invoice:   invoiceController.RestoreInvoice,
}

// Restore is POST /restore/{kind}/{id}.
func Restore(w http.ResponseWriter, r *http.Request) {
	k, known := softDelete.ParseKind(chi.URLParam(r, "kind"))
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	restore := kinds[k]
	if !known || restore == nil || err != nil {
		helpers.WriteNotFound(w, "There's nothing to restore here.")
		return
	}
	owner, restorable, err := softDelete.DeletedOwner(r.Context(), k, id)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Restore %s err: %+v", k, err)
		helpers.WriteRetryLater(w, "Couldn't restore it.")
		return
	}
	if !restorable {
		helpers.WriteNotFound(w, "There's nothing to restore: it's back already, or it was never deleted.")
		return
	}
	restore(w, r, id, owner)
}
