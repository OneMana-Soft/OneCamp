package controllers

// GET /admin/system-check — does this installation actually work?
//
// WHY AN ADMIN CAN ASK THIS AT ALL. Somebody who has just stood up fifteen
// services has no way to learn that a subsystem is silently dead. This product
// has shipped features that never worked from the day they landed, and the way
// that was found each time was a person noticing something looked wrong. The
// onboarding checklist tells an admin what to DO. This tells them what WORKS.
//
// Runs every check the build registered, reports each one by name with what it
// does and does not prove, and never mutates anything.

import (
	"net/http"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// perCheckTimeout bounds one probe. A subsystem that hangs is the case an admin
// most needs this for, so it must not stop the others being reported.
const perCheckTimeout = 8 * time.Second

// SystemCheckResponse is the whole answer, summarised so the page can lead with
// a number and expand into detail.
type SystemCheckResponse struct {
	Healthy   int                         `json:"healthy"`
	Unhealthy int                         `json:"unhealthy"`
	Total     int                         `json:"total"`
	CheckedAt int64                       `json:"checked_at"`
	Checks    []helpers.SystemCheckResult `json:"checks"`
}

// RunSystemCheck handles GET /admin/system-check.
func RunSystemCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	results := helpers.RunSystemChecks(ctx, perCheckTimeout)

	resp := SystemCheckResponse{
		Total:     len(results),
		CheckedAt: time.Now().Unix(),
		Checks:    results,
	}
	for _, c := range results {
		if c.Healthy {
			resp.Healthy++
		} else {
			resp.Unhealthy++
		}
	}

	// Always 200. An unhealthy subsystem is the ANSWER to this question, not a
	// failure to answer it, and a non-2xx would make the page render an error
	// instead of the diagnosis the admin came for.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": resp})
}
