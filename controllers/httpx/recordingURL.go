package httpx

// How a recording is answered to whoever may play it, whichever place it
// belongs to: a channel's, a DM's, a group's (controllers/User) or a calendar
// event's (controllers/Calendar, meet-02). Each route decides who may; this
// answers the same way for all of them.

import (
	"net/http"
	"time"

	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// WriteRecordingRefusal answers a recording that may not be played.
func WriteRecordingRefusal(w http.ResponseWriter, code int) {
	msg := "Recording not found"
	if code == http.StatusForbidden {
		msg = "Not authorised"
	}
	helpers.WriteJSON(w, code, helpers.Envolope{"msg": msg})
}

// RecordingUnavailable is what people see when a recording can't be read.
const RecordingUnavailable = "The recording couldn't be opened just now."

// TranscriptUnavailable is what people see when a call's transcript can't be
// read.
const TranscriptUnavailable = "The transcript couldn't be opened just now."

// WriteRecordingURL answers a recording's playback link, or why there is
// none to play: still being saved, failed, empty (it captured nothing), or a
// call's transcript with no recording. The link lives for the recording's length and
// half an hour (userBusiness.RecordingURLTTL), and expires_at says when, so the
// player fetches a new one before or after it runs out, keeping its place.
func WriteRecordingURL(w http.ResponseWriter, r *http.Request, rec *dgraphStruct.DgraphRecording) {
	switch {
	case rec.TranscriptOnly:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "transcript_only"})
		return
	case rec.EndedAt == nil || !rec.EndedAt.After(time.Unix(0, 0)):
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "recording"})
		return
	case recordingBusiness.Failed(rec.Status), recordingBusiness.Empty(rec.Status):
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": rec.Status})
		return
	}

	ttl := userBusiness.RecordingURLTTL(rec.Duration)
	url, err := userBusiness.GetRecordingURLByObjectName(r.Context(), rec.ObjectKey, ttl)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/WriteRecordingURL Failed to get url err: %+v", err)
		helpers.WriteRetryLater(w, RecordingUnavailable)
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":        "Got the url!",
		"url":        url,
		"expires_at": time.Now().Add(ttl).UnixMilli(),
		"status":     "ready",
	})
}
