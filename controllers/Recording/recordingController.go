package controllers

import (
	"context"
	"errors"
	"net/http"

	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// recordingUnreadable is what people see when a recording couldn't be read to
// delete it or bring it back: a failed read is not "not found" (the recording
// is still there).
const recordingUnreadable = "The recording couldn't be read just now."

// The reads and writes behind a conversation's recording's delete and restore
// (seams for tests).
var (
	liveChannelRecording    = recordingDomain.GetDgraphChannelRecordingInfoByEgressId
	deletedChannelRecording = recordingDomain.GetDeletedChannelRecordingInfo
	liveChatRecording       = recordingDomain.GetDgraphDmRecordingInfoByEgressId
	deletedChatRecording    = recordingDomain.GetDeletedDmRecordingInfo
	deleteRecording         = recordingBusiness.DeleteRecording
	restoreRecording        = recordingBusiness.RestoreRecording
)

// HandleDeleteChannelRecording deletes a channel's recording. The delete is
// soft: the channel's list and the player stop showing it at once, its Undo
// is HandleRestoreChannelRecording, and the workspace's purge setting removes
// it for good, file first. Only the channel's moderators and a workspace
// admin in the channel can delete.
func HandleDeleteChannelRecording(w http.ResponseWriter, r *http.Request) {
	changeRecording(w, r, "HandleDeleteChannelRecording", liveChannelRecording, inChannel, "delete",
		deleteRecording, "Recording deleted", "Recording not found")
}

// HandleRestoreChannelRecording brings back a channel's recording that
// HandleDeleteChannelRecording deleted (its Undo), for the same people.
func HandleRestoreChannelRecording(w http.ResponseWriter, r *http.Request) {
	changeRecording(w, r, "HandleRestoreChannelRecording", deletedChannelRecording, inChannel, "restore",
		restoreRecording, "Recording restored", "There's no deleted recording to bring back.")
}

// HandleDeleteChatRecording deletes a DM's or a group's recording, softly, as
// a channel's. DM: both participants can delete. Group: any member can delete.
func HandleDeleteChatRecording(w http.ResponseWriter, r *http.Request) {
	changeRecording(w, r, "HandleDeleteChatRecording", liveChatRecording, inChat, "delete",
		deleteRecording, "Recording deleted", "Recording not found")
}

// HandleRestoreChatRecording brings back a DM's or a group's recording that
// HandleDeleteChatRecording deleted (its Undo), for the same people.
func HandleRestoreChatRecording(w http.ResponseWriter, r *http.Request) {
	changeRecording(w, r, "HandleRestoreChatRecording", deletedChatRecording, inChat, "restore",
		restoreRecording, "Recording restored", "There's no deleted recording to bring back.")
}

// recordingRule is who may delete a recording in its home, and bring it back:
// filed is false when the recording isn't filed there or the asker can't see
// it there (not in its channel or conversation), who is answered as if there
// were none (V-1126); refused is the 403's words ("Only ... can <verb> its
// recordings.") for one who can see it and may not change it.
type recordingRule func(rec *dgraphStruct.DgraphRecording, who *userModel.UserInfo, verb string) (filed bool, refused string)

// inChannel: whoever is in the channel (ReaderIsIn) sees its recordings, and
// of them its moderators and a workspace admin may change them; its other
// members may only watch. Anyone else can't see them, a workspace admin outside
// it too (V-1166), as on their link and transcript.
func inChannel(rec *dgraphStruct.DgraphRecording, who *userModel.UserInfo, verb string) (bool, string) {
	switch {
	case rec.Channel == nil || !rec.ReaderIsIn(""):
		return false, ""
	case rec.Channel.IsAdmin != 0 || who.UserPostgresInfo.IsAdmin:
		return true, ""
	}
	return true, "Only the channel's moderators can " + verb + " its recordings."
}

// inChat: anyone in the conversation; anyone else can't see its recordings.
func inChat(rec *dgraphStruct.DgraphRecording, _ *userModel.UserInfo, _ string) (bool, string) {
	return rec.Dm != nil && rec.ReaderIsIn(""), ""
}

// changeRecording is all four: read the recording in the state the change
// needs (live to delete it, deleted to bring it back), check the home's rule,
// then write it, answered in the change's own words: done, or missing when
// there is nothing to change or the asker can't see it (V-1126). A read or a
// write that failed is "try again". Every answer is {"msg": ...}, the one
// shape the web reads (the delete's 400 and 404 were {"error": ...}, which it
// never showed).
func changeRecording(w http.ResponseWriter, r *http.Request, where string,
	read func(ctx context.Context, egressId, userDgraphUID string) (*dgraphStruct.DgraphRecording, error),
	rule recordingRule, verb string, write func(ctx context.Context, egressId string) error, done, missing string) {
	ctx := r.Context()

	userInfo, ok := userModel.FromContext(ctx)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	egressId := chi.URLParam(r, "egressId")
	if egressId == "" {
		helpers.WriteError(w, http.StatusBadRequest, "egressId is required")
		return
	}

	rec, err := read(ctx, egressId, userInfo.UserDgraphInfo.Uid)
	if err != nil && !errors.Is(err, recordingDomain.ErrNotFound) {
		helpers.WriteRetryLater(w, recordingUnreadable)
		return
	}
	filed, refused := false, ""
	if rec != nil {
		filed, refused = rule(rec, userInfo, verb)
	}
	if !filed {
		helpers.WriteNotFound(w, missing)
		return
	}
	if refused != "" {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": refused})
		return
	}

	if err := write(ctx, egressId); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/%s failed: %+v", where, err)
		helpers.WriteRetryLater(w, "The recording couldn't be changed just now.")
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": done})
}
