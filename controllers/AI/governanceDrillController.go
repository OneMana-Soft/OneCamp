package controllers

// Admin endpoints for the governance drill. They sit beside the audit-log
// endpoints and behind the same admin gate, because the drill writes to that log
// and its answer is about that log.

import (
	"math"
	"net/http"
	"strconv"
	"time"

	drillBusiness "github.com/akashc777/OneCamp/business/AIDrill"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// How often one person may run the drill.
//
// Not an authorisation control. Running the drill can only ever prove you
// cannot do something: it writes two rows attributed to you, rows you can
// already read, and attempts an action the executor refuses. What is worth
// bounding is the recomputation of the audit chain each run performs, which is
// the one part whose cost does not belong to the caller alone.
var memberDrillRuns = helpers.NewRateLimiter(5, time.Minute)

// GetGovernanceDrill handles GET /admin/governance-drill.
//
// Status only, so the UI can offer "set this up" rather than showing a failure a
// reader has to interpret. Never runs anything: a GET that wrote audit rows would
// fire on every page load and on every prefetch.
func GetGovernanceDrill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"seeded":            drillBusiness.Seeded(ctx),
		"allowed_channel":   drillBusiness.AllowedChannel,
		"forbidden_channel": drillBusiness.ForbiddenChannel,
	}})
}

// SetupGovernanceDrill handles POST /admin/governance-drill/setup.
//
// Creates the two namespaced channels and leaves the forbidden one. Idempotent,
// so a double click is not a second pair of channels.
func SetupGovernanceDrill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	if err := drillBusiness.Seed(ctx, userInfo); err != nil {
		// The error text names which half failed and why it matters, so it is
		// returned rather than replaced with a generic message: a half-built
		// fixture is the one state that would make the drill lie.
		helpers.LogErrorWithContext(ctx, "controllers/SetupGovernanceDrill failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"seeded":            true,
		"allowed_channel":   drillBusiness.AllowedChannel,
		"forbidden_channel": drillBusiness.ForbiddenChannel,
	}})
}

// RunGovernanceDrill handles POST /admin/governance-drill/run.
//
// A POST because it writes to the audit log; that is the point rather than a side
// effect. 200 even when the drill FAILS: the request succeeded and the finding is
// in the body, and a 500 would make a broken permission check look like a broken
// endpoint. Only a drill that could not run at all is an error.
func RunGovernanceDrill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	res, err := drillBusiness.Run(ctx, userInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/RunGovernanceDrill could not run err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// RunMyGovernanceDrill handles POST /ai/governance-drill/run.
//
// The same drill as the admin route, run as whoever is asking.
//
// WHY A MEMBER MAY RUN THIS. The drill lived behind the admin gate, and the
// people who most need to believe the guarantee are not admins. On the public
// demo nobody is: a visitor could read that agents are bounded by permissions
// and had no way to make one try. A refusal you watched happen is a different
// kind of evidence from a refusal you were shown, and this is the only place the
// product can offer the first kind.
//
// It grants nothing. Step one checks the caller is NOT in the channel and stops
// if they are, so the only thing a member can demonstrate here is a limit
// holding against themselves.
func RunMyGovernanceDrill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	actor := userInfo.UserPostgresInfo.Id.String()
	if status, msg, wait := memberDrillGate(drillBusiness.Seeded(ctx), memberDrillRuns, actor); status != 0 {
		if seconds := int(math.Ceil(wait.Seconds())); seconds > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	res, err := drillBusiness.Run(ctx, userInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/RunMyGovernanceDrill could not run err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// memberDrillGate decides whether a member's request gets as far as the drill.
// Returns a zero status when it should.
//
// A function rather than two ifs in the handler so the ORDER can be asserted: a
// workspace where the drill was never set up must not spend the caller's
// allowance. Otherwise five clicks on a button that was never going to work lock
// the person out of the one that would, once an admin sets it up.
func memberDrillGate(seeded bool, limiter *helpers.RateLimiter, actor string) (int, string, time.Duration) {
	// Seeding creates channels, which is an admin's decision, so a member who
	// arrives before the fixture exists is told who can make it rather than shown
	// an error about a missing channel.
	if !seeded {
		return http.StatusConflict,
			"The drill has not been set up on this workspace yet. An admin can create it from Admin settings.",
			0
	}
	if !limiter.Allow(actor) {
		return http.StatusTooManyRequests,
			"That is a few runs in quick succession. Give it a minute and try again.",
			limiter.Retry(actor)
	}
	return 0, "", 0
}
