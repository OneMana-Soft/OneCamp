package controllers

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/AI"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// DraftProjectUpdate handles POST /ai/project-update/draft {project_uuid, tz}: the
// project's next update drafted from its tasks, with an AI summary on top
// when the AI is available. For the project's admins, who post updates.
func DraftProjectUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := userModels.FromContext(ctx)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	var in struct {
		ProjectUUID string `json:"project_uuid"`
		// TZ is where the author is; days are counted there.
		TZ string `json:"tz"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which project."})
		return
	}
	projectID, err := uuid.Parse(in.ProjectUUID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a project."})
		return
	}
	// As the update's own routes: whoever posts the project's updates (Work
	// Access: its members and admins).
	if _, _, ok := projectaccess.RequireID(w, r, projectID.String(), workAccess.NeedPostUpdate, ""); !ok {
		return
	}
	d, err := business.DraftProjectUpdate(ctx, projectID, user.UserDgraphInfo.Uid, time.Now().In(helpers.Location(in.TZ)))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AI/DraftProjectUpdate err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't draft the update.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": d})
}
