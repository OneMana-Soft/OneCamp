package controllers

import (
	"net/http"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Attention GET /v1/attention, scope attention:read: the Home "what needs me"
// queue (approvals waiting, overdue tasks, commitments, today's calendar) for
// the token's owner. enabled is false when AI is off.
func Attention(w http.ResponseWriter, r *http.Request) {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	resp, err := aiBusiness.GetWhatNeedsMe(r.Context(), &userInfo)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to read what needs you"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": resp})
}
