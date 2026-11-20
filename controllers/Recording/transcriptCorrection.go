package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	transcriptExport "github.com/akashc777/OneCamp/business/TranscriptExport"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// The correction and the by-key export reach the business through these
// (seams for tests).
var (
	correctLine        = recordingBusiness.CorrectTranscriptLine
	readableTranscript = recordingBusiness.ReadableTranscript
)

// correctionBodyMost is the most a correction's body may be: a line's words
// twice over, with room.
const correctionBodyMost = 64 << 10

// HandleCorrectTranscriptLine corrects one line of a call's transcript:
// PATCH .../recordings/{key}/transcript/lines/{line_uid} {text, was}, in the
// app and through a write-scoped token. Who may: the line's speaker, and
// whoever may delete the recording (inChannel, inChat: the delete's own
// rule); everyone else who can read it is told so (403), and anyone who
// can't read it gets the missing line's 404. A line changed since the asker
// read it is a 409 with who changed it and its words now. 200: {"line"}.
func HandleCorrectTranscriptLine(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := userModel.FromContext(ctx)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		Text string  `json:"text"`
		Was  *string `json:"was"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, correctionBodyMost)).Decode(&body); err != nil || body.Was == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the line's new words and the words you saw."})
		return
	}
	who := recordingBusiness.Corrector{UserUID: userInfo.UserDgraphInfo.Uid, UserUUID: userInfo.UserDgraphInfo.Uuid,
		Name: userInfo.UserDgraphInfo.UserFullName, Deletes: mayDelete(ctx, userInfo)}
	line, err := correctLine(ctx, who, recordingBusiness.Correction{Key: chi.URLParam(r, "key"), LineUID: chi.URLParam(r, "line_uid"),
		Text: body.Text, Was: *body.Was})
	var stale *recordingBusiness.StaleLine
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"line": line})
	case errors.Is(err, recordingBusiness.ErrLineMissing):
		helpers.WriteNotFound(w, "That line isn't in this transcript.")
	case errors.Is(err, recordingBusiness.ErrMayNotCorrect):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the person who said this line, and the people who can delete the recording, can correct it."})
	case errors.As(err, &stale):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": stale.Message(), "line": stale.Line})
	case errors.Is(err, recordingBusiness.ErrBadCorrection):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A line's words can't be empty, or longer than 4,000 characters."})
	default:
		// Ids only: a store's error may echo the words it was given.
		helpers.LogErrorWithContext(ctx, "controllers/HandleCorrectTranscriptLine: line %s of %s wasn't corrected", chi.URLParam(r, "line_uid"), chi.URLParam(r, "key"))
		helpers.WriteRetryLater(w, "The line couldn't be corrected just now.")
	}
}

// mayDelete is the recording's delete rule as its routes apply it to who:
// a channel's moderators and workspace admins in it (inChannel), anyone in a chat
// (inChat). Through an API token a workspace admin is only a member
// (helpers.ActsThroughToken), as on every route a token reaches.
func mayDelete(ctx context.Context, who *userModel.UserInfo) func(*dgraphStruct.DgraphRecording) bool {
	asked := *who
	if helpers.ActsThroughToken(ctx) {
		asked.UserPostgresInfo.IsAdmin = false
	}
	return func(rec *dgraphStruct.DgraphRecording) bool {
		rule := inChat
		if rec.Channel != nil {
			rule = inChannel
		}
		filed, refused := rule(rec, &asked, "delete")
		return filed && refused == ""
	}
}

// HandleExportTranscript answers a call's transcript as a file by its
// recording's key: GET .../recordings/{key}/transcript?format=vtt|srt|txt, in
// the app and through a read-scoped token. One capped read; whoever can't
// read the transcript gets the missing answer.
func HandleExportTranscript(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := userModel.FromContext(ctx)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if _, ok := transcriptExport.ParseFormat(r.URL.Query().Get("format")); !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Ask for format=vtt, srt or txt."})
		return
	}
	// With its format asked, Answer answers the file (in ?lang= too) itself.
	transcriptExport.Answer(w, r, helpers.Page{Size: transcriptExport.MaxLines}, func(helpers.Page) (*dgraphStruct.DgraphRecording, string) {
		rec, err := readableTranscript(ctx, userInfo.UserDgraphInfo.Uid, chi.URLParam(r, "key"), transcriptExport.MaxLines)
		switch {
		case errors.Is(err, recordingBusiness.ErrLineMissing):
			helpers.WriteNotFound(w, "There's no transcript here.")
		case err != nil:
			helpers.LogErrorWithContext(ctx, "controllers/HandleExportTranscript: the transcript of %s wasn't read", chi.URLParam(r, "key"))
			helpers.WriteRetryLater(w, "The transcript couldn't be read just now.")
		default:
			return rec, transcriptExport.PlaceOf(rec)
		}
		return nil, ""
	})
}
