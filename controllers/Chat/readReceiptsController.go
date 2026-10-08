package controllers

// Read receipts in DMs and group chats (business/Chat/readReceipts.go): who
// has seen a conversation, and marking it seen while it's open.

import (
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Chat"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// receiptsTarget is the conversation a request names, by the other person's
// uuid for a DM ({user_uuid}) or by its grouping id for a group ({grp_id}).
func receiptsTarget(r *http.Request, me *userModels.UserInfo) (groupingID string, dm bool, ok bool) {
	if other := chi.URLParam(r, "user_uuid"); other != "" {
		id, err := uuid.Parse(other)
		if err != nil {
			return "", true, false
		}
		return helpers.GetGroupingId(me.UserDgraphInfo.Uuid, id.String()), true, true
	}
	grp := chi.URLParam(r, "grp_id")
	return grp, false, grp != ""
}

// GetChatReceipts handles GET /dm/seen/{user_uuid} and
// /groupChat/seen/{grp_id}: {on, seen: [{user_uuid, seen_at}]}, the others
// in the conversation who have seen it and up to when. A DM that hasn't
// started yet has none.
func GetChatReceipts(w http.ResponseWriter, r *http.Request) {
	me := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	grp, dm, ok := receiptsTarget(r, &me)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Name a conversation."})
		return
	}
	receipts, err := business.ChatReceipts(r.Context(), &me, grp)
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": receipts})
	case errors.Is(err, business.ErrNotInChat) && dm:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.Receipts{Seen: []business.SeenBy{}}})
	case errors.Is(err, business.ErrNotInChat):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "You're not in this conversation."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/GetChatReceipts err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't read who has seen it."})
	}
}

// MarkChatSeen handles POST /dm/seen/{user_uuid} and
// /groupChat/seen/{grp_id}: the person has the conversation open now. The
// others who'd see the receipt hear of it live.
func MarkChatSeen(w http.ResponseWriter, r *http.Request) {
	me := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	grp, dm, ok := receiptsTarget(r, &me)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Name a conversation."})
		return
	}
	err := business.MarkChatSeen(r.Context(), &me, grp)
	switch {
	case err == nil, errors.Is(err, business.ErrNotInChat) && dm:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Seen."})
	case errors.Is(err, business.ErrNotInChat):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "You're not in this conversation."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/MarkChatSeen err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't mark it seen."})
	}
}
