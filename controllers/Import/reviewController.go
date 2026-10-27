package controllers

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	reviewBusiness "github.com/akashc777/OneCamp/business/ImportReview"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
)

// ListImportReviews GET /admin/import-reviews: each project old Todoist and
// Notion imports added people to, with how many are undecided, taken off and
// kept, and how many are undecided in all. Admins only.
func ListImportReviews(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.RequireAdmin(w, r); !ok {
		return
	}
	projects, pending, err := reviewBusiness.List(r.Context())
	if reviewRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ListImportReviews err: %+v", err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't load who old imports added. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"projects": projects, "pending": pending})
}

// ListImportReviewPeople GET /admin/import-reviews/projects/{project_uuid}?after=<review id>:
// a page of the people old imports added to the project, by name, and
// whether more follow. Admins only.
func ListImportReviewPeople(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.RequireAdmin(w, r); !ok {
		return
	}
	project, ok := helpers.UUIDParam(w, r, "project_uuid")
	if !ok {
		return
	}
	var after *uuid.UUID
	if s := r.URL.Query().Get("after"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			helpers.WriteError(w, http.StatusBadRequest, "That page isn't one this list gave.")
			return
		}
		after = &id
	}
	people, more, err := reviewBusiness.People(r.Context(), project, after)
	if reviewRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ListImportReviewPeople %s err: %+v", project, err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't load who old imports added. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"people": people, "more": more})
}

// RemoveImportReview POST /admin/import-reviews/{id}/remove: takes the person
// a review names off its project. Admins only.
func RemoveImportReview(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.RequireAdmin(w, r); !ok {
		return
	}
	id, ok := helpers.UUIDParam(w, r, "id")
	if !ok {
		return
	}
	err := reviewBusiness.Remove(r, id)
	if reviewRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/RemoveImportReview %s err: %+v", id, err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't take them off the project. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Taken off the project."})
}

// RemoveAllImportReviews POST /admin/import-reviews/projects/{project_uuid}/remove-all:
// takes up to a batch of the people still undecided in the project off it, and
// answers how many went and how many are left, for the app to call again while
// any are. Admins only.
func RemoveAllImportReviews(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.RequireAdmin(w, r); !ok {
		return
	}
	project, ok := helpers.UUIDParam(w, r, "project_uuid")
	if !ok {
		return
	}
	n, left, err := reviewBusiness.RemoveAll(r, project)
	if reviewRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/RemoveAllImportReviews %s after %d err: %+v", project, n, err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't take everyone off the project. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Taken off the project.", "removed": n, "left": left})
}

// KeepImportReviews POST /admin/import-reviews/projects/{project_uuid}/keep:
// keeps everyone still undecided in the project. Admins only.
func KeepImportReviews(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.RequireAdmin(w, r); !ok {
		return
	}
	project, ok := helpers.UUIDParam(w, r, "project_uuid")
	if !ok {
		return
	}
	n, err := reviewBusiness.Keep(r, project)
	if reviewRefused(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/KeepImportReviews %s err: %+v", project, err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Couldn't keep them. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Kept in the project.", "kept": n})
}

// reviewRefused answers a review action the business refused, and says
// whether it was one.
func reviewRefused(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, reviewBusiness.ErrThroughToken):
		helpers.WriteForbidden(w, "Review who an import added in the app.")
	case errors.Is(err, reviewBusiness.ErrNotAdmin):
		helpers.WriteForbidden(w, "Only workspace admins can do that.")
	case errors.Is(err, reviewBusiness.ErrNoReview):
		helpers.WriteNotFound(w, "That review isn't there any more.")
	case errors.Is(err, reviewBusiness.ErrDecided):
		helpers.WriteError(w, http.StatusConflict, "An admin has already decided that one.")
	case errors.Is(err, reviewBusiness.ErrCreator):
		helpers.WriteForbidden(w, "The project's creator can't be taken off it.")
	default:
		return false
	}
	return true
}
