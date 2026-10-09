package controllers

import (
	"encoding/json"
	"net/http"

	mqttAccess "github.com/akashc777/OneCamp/business/MqttAccess"
	"github.com/akashc777/OneCamp/helpers"
)

// AuthorizeMqtt POST /internal/mqtt/authorize: the broker asks before every
// subscription and publish a person's client makes (emqx's HTTP authorization
// source), with the internal secret. The answer is emqx's: {"result":
// "allow"} or {"result": "deny"}; anything else, or no answer within its
// timeout, is a deny too.
func AuthorizeMqtt(w http.ResponseWriter, r *http.Request) {
	var req mqttAccess.Request
	result := "deny"
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err == nil &&
		mqttAccess.Allowed(r.Context(), req) {
		result = "allow"
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"result": result})
}
