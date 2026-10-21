// Package controllers (Cards) is the press on a card (business/Cards): the
// web sends back the card's token with the action pressed, and the server
// checks the press before its owner hears of it.
package controllers

import (
	"encoding/json"
	"errors"
	"net/http"

	cardsBusiness "github.com/akashc777/OneCamp/business/Cards"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// pressesPerMinute bounds one member's presses, as their commands are.
const pressesPerMinute = 60

// Act is POST /cards/act: {token, action_id, value}.
func Act(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, ok := userModels.FromContext(ctx)
	if !ok || u == nil {
		helpers.WriteUnauthorized(w)
		return
	}
	var body struct {
		Token    string `json:"token"`
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.Token == "" || body.ActionID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the card's token and the action pressed."})
		return
	}
	if res := redisStore.AllowFixedWindowOrLocal(ctx, registry.CommandRate, []string{u.UserDgraphInfo.Uuid}, pressesPerMinute); !res.Allowed {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "Too many presses. Wait a moment, then try again."})
		return
	}
	reply, err := cardsBusiness.Act(ctx, cardsBusiness.Who{UserUUID: u.UserDgraphInfo.Uuid}, body.Token, body.ActionID, body.Value)
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": reply})
	case errors.Is(err, cardsBusiness.ErrExpired):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "This card has expired. Run the command again for a fresh one."})
	case errors.Is(err, cardsBusiness.ErrBadToken), errors.Is(err, cardsBusiness.ErrNoAction):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't one of this card's actions."})
	case errors.Is(err, cardsBusiness.ErrNotYours):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't act on this card."})
	case errors.Is(err, cardsBusiness.ErrNoOwner):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "Nothing answers this card any more: its app may have been turned off or removed."})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/Cards/Act: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't send that. Try again in a moment."})
	}
}
