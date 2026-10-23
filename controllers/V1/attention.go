package controllers

import (
	"net/http"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// /attention is the AI edition's (routes.go registers the rest of /v1).
func init() {
	v1api.Register(v1api.Resource{
		Name: "attention", Owner: "platform",
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/attention", Summary: "What needs you: approvals, overdue tasks, commitments, today", Scope: apiTokenBusiness.ScopeAttentionRead,
				AppsRefused: "What needs you is a person's list; an app has none.", Legacy: true, Handler: Attention},
		},
	})
}

// Attention GET /v1/attention, scope attention:read: the Home "what needs me"
// queue (approvals waiting, overdue tasks, commitments, today's calendar) for
// the token's owner. enabled is false when AI is off.
func Attention(w http.ResponseWriter, r *http.Request) {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	// ?tz= is the reader's zone: today's meetings are the day their calendar shows.
	resp, err := aiBusiness.GetWhatNeedsMe(aiBusiness.WithReaderZone(r.Context(), r.URL.Query().Get("tz")), &userInfo)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to read what needs you"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": resp})
}
