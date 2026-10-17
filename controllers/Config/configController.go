package controllers

import (
	"net/http"

	business "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func GetMqttConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	mqttConfig, err := business.GetMqttConfig(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserPostgresInfo.IsAdmin)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetMqttConfig Failed to get user mqtt config err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't start live updates. Reload the page to try again.",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got the live update settings.", "data": mqttConfig})
}
