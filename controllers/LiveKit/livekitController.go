package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"

	adapter "github.com/akashc777/OneCamp/adapter/LiveKit"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	business "github.com/akashc777/OneCamp/business/LiveKit"
	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"google.golang.org/protobuf/encoding/protojson"
)

func HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Create a KeyProvider
	keyProvider := auth.NewSimpleKeyProvider(
		livekitInit.LiveKitService.Config.ApiKey,
		livekitInit.LiveKitService.Config.ApiSecret,
	)

	// Receive and validate the webhook event
	bytes, err := webhook.Receive(r, keyProvider)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleWebhook Error validating webhook event: %v", err)
		http.Error(w, "Webhook validation failed", http.StatusUnauthorized)
		return
	}

	var event livekit.WebhookEvent
	// Use DiscardUnknown to tolerate new protobuf fields from newer LiveKit versions
	// (e.g., "repairSsrc" added in v1.11.x) without breaking the webhook handler.
	unmarshaller := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaller.Unmarshal(bytes, &event); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleWebhook Error unmarshalling webhook event: %v", err)
		http.Error(w, "Webhook parsing failed", http.StatusInternalServerError)
		return
	}

	// When it happened, as LiveKit says: Call State orders joins and leaves by
	// it, so a webhook that arrives late or twice changes nothing it shouldn't.
	at := time.Now()
	if event.CreatedAt > 0 {
		at = time.Unix(event.CreatedAt, 0)
	}

	// Handle specific events. A LiveKit server shared between workspaces tells
	// every one of them about every room: a room that isn't ours is left alone.
	switch event.Event {
	case "egress_ended":
		recordingEnded(ctx, event.EgressInfo)

	case "room_finished":
		if event.Room == nil || !callStateBusiness.BelongsHere(ctx, event.Room.Name) {
			break
		}
		roomName := event.Room.Name
		helpers.LogInfoWithContext(ctx, "Room finished: %s", roomName)
		callControlBusiness.OnEnded(ctx, roomName, event.Room.Sid, callStateModel.EndedRoomFinished)
		broadcastCallStop(ctx, roomName)
		callFinished(ctx, event.Room)

	case "participant_joined":
		// Who joined this call, their part in it, and a check that they may be
		// in it now, since a token outlives the right it was minted for
		// (removed from the channel, a link revoked, put out by a host, a call
		// locked since): business/CallControl.
		if event.Room == nil || event.Participant == nil || !callStateBusiness.BelongsHere(ctx, event.Room.Name) {
			break
		}
		callControlBusiness.OnJoined(ctx, event.Room.Name, event.Room.Sid, event.Participant, at)

	case "participant_left":
		// Clear the call indicator the moment the LAST human leaves, instead
		// of waiting for room_finished — which only fires after LiveKit's
		// departure timeout AND after the transcription agent (a participant)
		// also leaves, so it can lag by many seconds or, if the agent lingers,
		// effectively never arrive. This is the primary fix for the call dot
		// that stays lit (and, for DMs, never clears) after a call ends.
		if event.Room == nil || event.Participant == nil || business.IsAgentIdentity(event.Participant.Identity) {
			break
		}
		roomName := event.Room.Name
		if !callStateBusiness.BelongsHere(ctx, roomName) {
			helpers.LogInfoWithContext(ctx, "controllers/HandleWebhook Skipping participant_left for non-local room: %s", roomName)
			break
		}
		callControlBusiness.OnLeft(ctx, roomName, event.Room.Sid, event.Participant, at)

		// Only broadcast "call stopped" if no human participants remain.
		// Exclude the participant who just left — LiveKit may still list
		// them briefly depending on event ordering. A transient
		// ListParticipants error is treated as "still active" so we never
		// falsely clear a live call; room_finished remains the backstop.
		active, lerr := business.IsCallActiveExcluding(ctx, roomName, event.Participant.Identity)
		if lerr != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/HandleWebhook participant_left active-check failed for %s err: %+v",
				roomName, lerr)
			break
		}
		if active {
			// Other humans are still in the call; nothing to do.
			break
		}

		helpers.LogInfoWithContext(ctx, "Last human left, ending call: %s", roomName)
		callControlBusiness.OnEnded(ctx, roomName, event.Room.Sid, callStateModel.EndedLastLeft)
		broadcastCallStop(ctx, roomName)

		// Best-effort: evict the room so the lingering transcription
		// agent disconnects promptly and LiveKit reclaims resources,
		// rather than holding the room open for its departure timeout.
		business.DeleteRoom(ctx, roomName)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// recordingEnded stamps a recording's end, length, size and how it ended.
//
// A failed or aborted egress (out of memory on a small box, the room closed
// under it, storage refused the upload) used to be stamped like a good one: it
// was listed at full length and played a black frame. Its status is kept now,
// and lists and the player say it failed. A webhook without its egress is
// ignored rather than dereferenced.
func recordingEnded(ctx context.Context, info *livekit.EgressInfo) {
	if info == nil || strings.TrimSpace(info.EgressId) == "" {
		return
	}
	var durationSec float64
	var fileSize int64
	for _, file := range info.FileResults {
		if file == nil {
			continue
		}
		durationSec = float64(file.Duration) / 1e9
		fileSize = file.Size
	}
	if durationSec == 0 && info.EndedAt > info.StartedAt {
		durationSec = float64(info.EndedAt-info.StartedAt) / 1e9
	}
	status := recordingBusiness.StatusFromEgress(info.Status, fileSize)
	if recordingBusiness.Failed(status) {
		// LiveKit's reason goes to the log, never on screen.
		helpers.LogErrorWithContext(ctx, "controllers/HandleWebhook recording %s ended %s: %s", info.EgressId, info.Status, info.Error)
	}
	_ = recordingBusiness.UpdateStopRecordingTime(ctx, info.EgressId, durationSec, fileSize, info.StartedAt, status)
	// The call's record lists it with how it ended.
	if err := callStateModel.RecordingEnded(ctx, info.EgressId, status, time.Now()); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleWebhook could not note the end of recording %s on its call: %+v", info.EgressId, err)
	}
	// Its transcript lines go back to the call's own session from here.
	business.ForgetRecording(ctx, info.RoomName, info.EgressId)
}

// callFinished closes the books on the call that just ended in a room: its
// transcript gets an end (so it is listed, deletable and under retention), the
// switches and removals kept for it are dropped, and, if anybody actually
// joined it, the recap and the "meeting ended" workflows run.
//
// "If anybody joined": the room is made when the first token is minted, and a
// room nobody ever connected to closes empty. That is no meeting, and treating
// it as one fired "meeting ended" workflows and a recap for a call that never
// happened.
func callFinished(ctx context.Context, room *livekit.Room) {
	call := business.FinishedCall{
		Room:       room.Name,
		SessionKey: helpers.CallSessionKeyFor(room.Sid),
		Ended:      time.Now(),
	}
	if room.CreationTime > 0 {
		call.Began = time.Unix(room.CreationTime, 0)
	}
	business.EndCallSession(ctx, call.SessionKey, call.Began)
	business.ForgetCall(ctx, call.Room)

	attendees, err := business.CallAttendees(ctx, call.SessionKey)
	if err == nil && len(attendees) == 0 {
		helpers.LogInfoWithContext(ctx, "controllers/HandleWebhook nobody joined the call in %s; no recap or workflows", call.Room)
		return
	}

	// Ambient AI: turn the finished call's transcript into a recap
	// (summary + decisions + action items) posted to the surface the
	// call happened in. Fire-and-forget; self-gates on the
	// meeting_recap_enabled setting and is idempotent per call.
	//
	// Skipped for instant meetings (meet-<uuid>): they are not tied to
	// a channel/DM/group conversation, so there is no surface to post
	// a recap into.
	if !guestBusiness.IsMeetingRoom(call.Room) {
		aiBusiness.MaybeRunMeetingRecap(call)
	}

	// A finished call is the workspace event most likely to produce
	// work — a decision to record, a task somebody agreed to, a status
	// update the team is waiting on — and until now it was the only one
	// nothing could react to. Detached and panic-guarded inside, so a
	// workflow can never hold up or crash call teardown. Present on
	// both editions: the workflow engine is not an AI feature.
	workflowBusiness.NotifyMeetingEnded(call.Room)
}

// Room naming conventions (business/CallState rooms.go):
//   - DM call:     space-separated sorted UUIDs ("uuidA uuidB")
//   - Group chat:  32-char hex hash (no hyphens, no spaces)
//   - Channel:     a UUID (hyphens, no spaces)
func broadcastCallStop(ctx context.Context, roomName string) {
	switch {
	case guestBusiness.IsMeetingRoom(roomName):
		// Instant meeting: not pinned to a sidebar conversation, so there is
		// no call-active dot to clear. Guests/host observe call end via
		// LiveKit's own disconnect events. Nothing to broadcast.
		return
	case strings.Contains(roomName, " "):
		// DM call
		chatBusiness.PublishStopChatCall(roomName)
	case !strings.Contains(roomName, "-"):
		// Group chat call (hash)
		chatBusiness.PublishStopChatCall(roomName)
	default:
		// Channel call (UUID)
		channelBusiness.PublishChannelCallStop(roomName)
	}
}

// transcriptRefusal answers a line the server declines to keep, with the code
// the client acts on (see the CALLS-FE ↔ CALLS-BE contract). Not a 500: each
// is a normal answer, and the first is also what a browser sees for a beat
// after everyone hangs up.
func transcriptRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, business.ErrNotInCall), errors.Is(err, business.ErrNotASpeaker):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "not_in_call", "msg": "You're not in this call."})
	case errors.Is(err, business.ErrTranscriptionDisabled):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "transcription_disabled", "msg": "Transcription is turned off for this workspace."})
	case errors.Is(err, business.ErrTranscriptionOff):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "transcription_off", "msg": "Transcription isn't on in this call."})
	case errors.Is(err, business.ErrTranscriptNotKept):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "transcript_not_kept", "msg": "This call keeps no transcript."})
	case errors.Is(err, business.ErrNotBrowserMode):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "not_browser_mode", "msg": "This workspace transcribes calls another way."})
	default:
		return false
	}
	return true
}

// SaveMyTranscript POST /livekit/my-transcript — a participant persisting their
// OWN speech, from browser transcription mode, while the call's transcription
// is on.
//
// The identity is taken from the SESSION, never from the body: a caller can
// only ever attribute words to themselves. Presence in the room is checked
// against LiveKit, so they cannot post into a call they are not in, and the
// call is resolved server-side rather than accepted, so they cannot choose
// which call their words land in. And nothing is kept unless somebody turned
// transcription on in the call, which told everyone in it.
func SaveMyTranscript(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		RoomName string `json:"room_name"`
		Text     string `json:"text"`
		OffsetMs *int64 `json:"offset_ms,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That line couldn't be read."})
		return
	}

	text := strings.TrimSpace(input.Text)
	if text == "" || strings.TrimSpace(input.RoomName) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which call and what was said."})
		return
	}
	// Same cap as the agent path: a transcript chunk is a sentence, not a file.
	if len(text) > 16*1024 {
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{"msg": "That line is too long to keep."})
		return
	}

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	if !ok || strings.TrimSpace(userInfo.UserDgraphInfo.Uid) == "" {
		helpers.WriteUnauthorized(w)
		return
	}

	err := business.StoreCallLine(ctx, business.CallLine{
		RoomName:  input.RoomName,
		Identity:  userInfo.UserDgraphInfo.Uid,
		Text:      text,
		Timestamp: time.Now().UnixMilli(),
		OffsetMs:  input.OffsetMs,
	})
	if err != nil {
		if transcriptRefusal(w, err) {
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/SaveMyTranscript failed to save err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save transcript"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Transcript saved"})
}

// ReportTranscriptionCapability POST /livekit/my-capability — a participant
// telling the server their browser cannot transcribe them.
//
// In browser mode the Web Speech API does the transcribing, and it exists only
// in Chrome and Edge. A participant on Firefox or Safari contributes nothing,
// and nothing anywhere errors: the transcript just has their turns missing and
// the recap summarises half a conversation as though it were all of it.
//
// The identity comes from the session and presence is checked against
// LiveKit, so a caller can only report about themselves, in a call they are
// actually in. Kept for THAT call (its session), not the room: a room's caveat
// reached every later call in the channel that day.
func ReportTranscriptionCapability(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		RoomName      string `json:"room_name"`
		CanTranscribe bool   `json:"can_transcribe"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.RoomName) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which call."})
		return
	}

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	if !ok || strings.TrimSpace(userInfo.UserDgraphInfo.Uid) == "" {
		helpers.WriteUnauthorized(w)
		return
	}
	identity := userInfo.UserDgraphInfo.Uid

	// Reuses the transcript grant: same question (are you in this call), same
	// cached answer, so a capability report costs no extra LiveKit calls.
	sessionKey, err := business.ResolveTranscriptSession(ctx, input.RoomName, identity)
	if err != nil {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "not_in_call", "msg": "You're not in this call."})
		return
	}

	// Only the negative is recorded. "I can transcribe" is the assumption
	// everywhere else, so storing it would be a set of everyone in every call
	// to express the absence of a problem.
	if !input.CanTranscribe {
		if err := redisStore.AddToSet(ctx, registry.CallUntranscribed, []string{sessionKey}, identity); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/ReportTranscriptionCapability could not record: %+v", err)
		}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Noted."})
}

// SaveTranscript POST /livekit/transcript — a line the server's own
// transcriber (the agent) heard, behind the internal secret.
//
// The agent used to relay every lk.transcription data packet it saw, taking
// the speaker from a field inside the packet: any participant, a guest on a
// meeting link included, could publish a line naming somebody else and have it
// stored in their name, in the transcript, the AI recap and the notes. Now
// only lines the agent transcribed itself are accepted (source "stt": an old
// agent still relaying sends none), the speaker must be connected to the call
// and a workspace member, the line is filed under the call the server
// resolves rather than one the agent names, and only while the call's
// transcription is on.
func SaveTranscript(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input adapter.TranscriptInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SaveTranscript Failed to decode transcript request: %v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	if input.Source != adapter.TranscriptSourceSTT {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"code": "not_transcribed_here", "msg": "Only lines the transcriber heard itself are kept."})
		return
	}
	// Length caps so an abusive (or buggy) agent can't push oversized
	// payloads into Dgraph. Real transcripts are < 1 KB per chunk.
	if len(input.Text) > 16*1024 {
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{"msg": "That line is too long to keep."})
		return
	}

	err := business.StoreCallLine(ctx, business.CallLine{
		RoomName:   input.RoomName,
		Identity:   input.ParticipantIdentity,
		Text:       input.Text,
		Timestamp:  input.Timestamp,
		OffsetMs:   input.OffsetMs,
		FromServer: true,
	})
	if err != nil {
		if transcriptRefusal(w, err) {
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save transcript"})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Transcript saved"})
}

// SetCallTranscription POST /livekit/transcription — a member in a call turns
// its transcription on or off, for everyone in it.
//
// The one switch behind captions and the kept transcript: while it is off,
// nobody's browser runs a recogniser (in Chrome and Edge that sends the
// person's speech to Google's or Microsoft's speech service) and the server's
// transcriber stays idle. Turning it on writes it into the room's metadata, so
// every client, guests too, shows who turned it on and where the audio goes.
func SetCallTranscription(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		RoomName string `json:"room_name"`
		On       bool   `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.RoomName) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which call."})
		return
	}
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	if !ok || strings.TrimSpace(userInfo.UserDgraphInfo.Uid) == "" {
		helpers.WriteUnauthorized(w)
		return
	}
	sw, err := business.SetTranscription(ctx, input.RoomName, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.DisplayName(), input.On)
	if err != nil {
		if transcriptRefusal(w, err) {
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/SetCallTranscription failed: %+v", err)
		helpers.WriteRetryLater(w, "The call server couldn't answer just now.")
		return
	}
	action, summary := "call.transcription.off", "Turned transcription off in a call"
	if input.On {
		action, summary = "call.transcription.on", "Turned transcription on in a call"
	}
	auditBusiness.Record(r, action, auditBusiness.CategoryCalls, summary,
		map[string]interface{}{"room": input.RoomName, "where": sw.Where})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": sw})
}
