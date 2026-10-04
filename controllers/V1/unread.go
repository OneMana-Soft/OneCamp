package controllers

import (
	"net/http"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Unread GET /v1/unread, scope attention:read: what the token's owner has not
// read, with the busiest channels and conversations first. Built for desktop
// bars (the OneMana kit for Omarchy), and the same counts the sidebar shows.
func Unread(w http.ResponseWriter, r *http.Request) {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	s, err := userBusiness.GetUnreadSummary(r.Context(), userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to count unread messages"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}
