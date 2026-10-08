package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/AI"
	goalBusiness "github.com/akashc777/OneCamp/business/Goal"
	"github.com/akashc777/OneCamp/helpers"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// DraftGoalCheckIn handles POST /ai/goal-checkin/draft {goal_id, tz}: the
// goal's next check-in drafted from its projects, sub-goals and number, with
// an AI summary on top when the AI is available. For whoever may check in.
func DraftGoalCheckIn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := userModels.FromContext(ctx)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
		return
	}
	var in struct {
		GoalID string `json:"goal_id"`
		// TZ is where the owner is; days are counted there.
		TZ string `json:"tz"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which goal."})
		return
	}
	id, err := uuid.Parse(in.GoalID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a goal."})
		return
	}
	reader := goalBusiness.Reader{UUID: user.UserPostgresInfo.Id, DgraphUID: user.UserDgraphInfo.Uid, IsAdmin: user.UserPostgresInfo.IsAdmin}
	g, err := goalModel.Get(id)
	if err != nil || g == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That goal no longer exists."})
		return
	}
	if !reader.CanEdit(g) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the goal's owner, whoever made it, or a workspace admin can check in."})
		return
	}
	d, err := business.DraftGoalCheckIn(ctx, reader, id, time.Now().In(helpers.Location(in.TZ)))
	if errors.Is(err, goalBusiness.ErrNotFound) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That goal no longer exists."})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AI/DraftGoalCheckIn err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't draft the check-in. Try again in a moment."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": d})
}
