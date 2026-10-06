package controllers

import (
	"net/http"

	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// HandleDeleteChannelRecording soft-deletes a channel recording.
// Only channel admins can delete.
func HandleDeleteChannelRecording(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := userModel.FromContext(ctx)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	egressId := chi.URLParam(r, "egressId")
	if egressId == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "egressId is required"})
		return
	}

	dgraphRecording, err := recordingDomain.GetDgraphChannelRecordingInfoByEgressId(ctx, egressId, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphRecording == nil || dgraphRecording.Channel == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "Recording not found"})
		return
	}
	// The channel's moderators may delete its recordings, and so may a
	// workspace admin; everyone else, members included, may only watch them.
	if dgraphRecording.Channel.IsAdmin == 0 && !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the channel's moderators can delete its recordings."})
		return
	}

	err = recordingBusiness.SoftDeleteRecording(ctx, egressId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleDeleteChannelRecording failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "Failed to delete recording"})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Recording deleted"})
}

// HandleDeleteChatRecording soft-deletes a DM/group chat recording.
// DM: both participants can delete. Group: any member can delete.
func HandleDeleteChatRecording(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := userModel.FromContext(ctx)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	egressId := chi.URLParam(r, "egressId")
	if egressId == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "egressId is required"})
		return
	}

	dgraphRecording, err := recordingDomain.GetDgraphDmRecordingInfoByEgressId(ctx, egressId, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphRecording == nil || dgraphRecording.Dm == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "Recording not found"})
		return
	}
	// Anyone in the conversation may delete its recordings; nobody else.
	if dgraphRecording.Dm.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only people in this conversation can delete its recordings."})
		return
	}

	err = recordingBusiness.SoftDeleteRecording(ctx, egressId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleDeleteChatRecording failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "Failed to delete recording"})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Recording deleted"})
}
