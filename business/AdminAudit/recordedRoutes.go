package business

import (
	"context"
	"errors"
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
)

// RefusedUnrecorded answers a change that Recorded refused because its audit
// row couldn't be written, and says whether it did: 503 with a retry-later
// message, so the client tries again, never the route's own failure. Any
// other error is the route's to answer (V-243's "no record, no action").
func RefusedUnrecorded(w http.ResponseWriter, r *http.Request, err error, where string) bool {
	return RefusedUnrecordedIn(r.Context(), w, err, where)
}

// RefusedUnrecordedIn is RefusedUnrecorded for an answerer that holds the
// request's context rather than the request (a route family's shared error
// answer).
func RefusedUnrecordedIn(ctx context.Context, w http.ResponseWriter, err error, where string) bool {
	if !errors.Is(err, ErrNotRecorded) {
		return false
	}
	helpers.LogErrorWithContext(ctx, "%s audit err: %+v", where, err)
	helpers.WriteRetryLater(w, "That couldn't be recorded in the audit log, so it wasn't done.")
	return true
}

// MembershipMeta is the audit metadata of a change to who is in something, or
// who holds a role there: its uuid and name under kind ("team_uuid",
// "team_name") and the person's uuid ("user_uuid"). Every membership row names
// them alike, so an audit reads a team's and a channel's the same way.
func MembershipMeta(kind, containerUUID, containerName, memberUUID string) map[string]interface{} {
	meta := map[string]interface{}{kind + "_uuid": containerUUID, "user_uuid": memberUUID}
	if containerName != "" {
		meta[kind+"_name"] = containerName
	}
	return meta
}
