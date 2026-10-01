package controllers

import (
	"time"

	"context"
	"encoding/json"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"net/http"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/LiveKit"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	business "github.com/akashc777/OneCamp/business/LiveKit"
	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"google.golang.org/protobuf/encoding/protojson"
)

// roomBelongsToThisInstance checks whether a LiveKit room (identified by its name)
// belongs to this customer instance. This is critical for multi-tenant deployments
// where a shared LiveKit server sends webhooks to ALL backends.
//
// Room naming conventions:
//   - Channel calls: room name is a UUID (e.g. "550e8400-e29b-41d4-a716-446655440000")
//   - DM calls:      room name is space-separated sorted UUIDs (e.g. "uuid1 uuid2")
//   - Group chats:   room name is a 32-char hex hash (no hyphens, no spaces)
func roomBelongsToThisInstance(ctx context.Context, roomName string) bool {
	// Instant meeting (guest-shareable) — room name is "meet-<uuid>". Owned by
	// this instance iff a guest grant row exists for it.
	if guestBusiness.IsMeetingRoom(roomName) {
		exists, err := guestBusiness.MeetingRoomExists(ctx, roomName)
		if err != nil {
			return false
		}
		return exists
	}

	// Channel call — room name is a UUID with hyphens
	if strings.Contains(roomName, "-") && !strings.Contains(roomName, " ") {
		channelUUID, err := uuid.Parse(roomName)
		if err != nil {
			return false
		}
		channelInfo, err := channelDomain.GetChannelInfoByUUID(ctx, channelUUID)
		if err != nil || channelInfo == nil {
			return false
		}
		return true
	}

	// DM call — room name contains space-separated UUIDs
	if strings.Contains(roomName, " ") {
		parts := strings.Fields(roomName)
		if len(parts) < 2 {
			return false
		}
		// The room belongs to this instance if ANY of its participant UUIDs
		// exists in this instance's Postgres. Checking only the first UUID
		// (the lexicographically smaller one) would wrongly reject — and
		// silently drop the call-stop broadcast for — DMs whose first UUID
		// belongs to a different tenant in a shared-LiveKit deployment.
		for _, p := range parts {
			userUUID, err := uuid.Parse(p)
			if err != nil {
				continue
			}
			if checkUserExistsInPostgres(ctx, userUUID) {
				return true
			}
		}
		return false
	}

	// Group chat — room name is a 32-char hex hash
	if len(roomName) == 32 && !strings.Contains(roomName, "-") {
		// For group chats we cannot easily verify ownership from the hash alone.
		// However, the MQTT publish will go to this instance's own EMQX broker,
		// targeting a topic derived from this instance's JWT_SECRET. If the room
		// doesn't belong here, no client will be subscribed — effectively a no-op.
		// We allow processing to avoid blocking legitimate group chat webhooks.
		return true
	}

	// Unknown format — skip to be safe
	return false
}

// checkUserExistsInPostgres does a lightweight check for a user UUID in Postgres.
func checkUserExistsInPostgres(ctx context.Context, userUUID uuid.UUID) bool {
	userInfo, err := userDomain.GetUserByUUID(ctx, userUUID)
	if err != nil || userInfo == nil {
		return false
	}
	return true
}

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

	// Handle specific events
	switch event.Event {
	case "egress_ended":
		// egress_ended: UpdateStopRecordingTime looks up by egressId in Dgraph.
		// If the egressId doesn't exist in this instance's Dgraph, it's a no-op.
		var durationSec float64
		var fileSize int64
		if event.EgressInfo != nil {
			for _, file := range event.EgressInfo.FileResults {
				durationSec = float64(file.Duration) / 1e9
				fileSize = file.Size
			}
		}

		if durationSec == 0 && event.EgressInfo != nil && event.EgressInfo.EndedAt > event.EgressInfo.StartedAt {
			durationSec = float64(event.EgressInfo.EndedAt-event.EgressInfo.StartedAt) / 1e9
		}

		derivedStartedAt := event.EgressInfo.StartedAt
		recordingBusiness.UpdateStopRecordingTime(ctx, event.EgressInfo.EgressId, durationSec, fileSize, derivedStartedAt)

	case "room_finished":
		if event.Room != nil {
			roomName := event.Room.Name

			// ── Multi-tenant guard ──────────────────────────────────────
			// In a shared LiveKit setup, webhooks are sent to ALL backends.
			// Only process events for rooms that belong to THIS instance.
			if !roomBelongsToThisInstance(ctx, roomName) {
				helpers.LogInfoWithContext(ctx, "controllers/HandleWebhook Skipping room_finished for non-local room: %s", roomName)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
				return
			}

			helpers.LogInfoWithContext(ctx, "Room finished: %s", roomName)
			broadcastCallStop(ctx, roomName)

			// Ambient AI: turn the finished call's transcript into a recap
			// (summary + decisions + action items) posted to the surface the
			// call happened in. Fire-and-forget; self-gates on the
			// meeting_recap_enabled setting and is idempotent per room.
			// room_finished fires AFTER the transcription agent leaves, so
			// the transcript is fully written by this point.
			//
			// Skipped for instant meetings (meet-<uuid>): they are not tied to
			// a channel/DM/group conversation, so there is no surface to post
			// a recap into.
			// Meeting recap is an AI-edition feature. v1 has no recap agent.

			// The workflow trigger is NOT an AI feature and belongs here too: a
			// finished call is the workspace event most likely to produce work,
			// and on this edition it is the only thing that reacts to one.
			// Detached and panic-guarded inside, so a workflow can never hold up
			// or crash call teardown.
			workflowBusiness.NotifyMeetingEnded(roomName)
		}

	case "participant_left":
		// Clear the call indicator the moment the LAST human leaves, instead
		// of waiting for room_finished — which only fires after LiveKit's
		// EmptyTimeout AND after the transcription agent (a participant) also
		// leaves, so it can lag by many seconds or, if the agent lingers,
		// effectively never arrive. This is the primary fix for the call dot
		// that stays lit (and, for DMs, never clears) after a call ends.
		if event.Room != nil && event.Participant != nil {
			roomName := event.Room.Name

			// Ignore the agent's own departure — its leaving doesn't change
			// whether a human call is in progress.
			if business.IsAgentIdentity(event.Participant.Identity) {
				break
			}

			// Record that this person was in the call, before anything below
			// can return early.
			//
			// This is the only event that fires for EVERY participant, because
			// a room cannot finish until they have all left, and room_finished
			// carries the room rather than its people. Without it the only
			// record of who attended is the transcript, which knows who spoke
			// and nothing about anyone who listened.
			//
			// Best-effort: a post-call artifact reaching fewer people is a
			// worse outcome than a call, so a Redis failure must never affect
			// teardown.
			if err := redisStore.AddToSet(ctx, registry.CallParticipants, []string{roomName}, event.Participant.Identity); err != nil {
				helpers.LogErrorWithContext(ctx, "controllers/HandleWebhook could not record call participant: %+v", err)
			}

			if !roomBelongsToThisInstance(ctx, roomName) {
				helpers.LogInfoWithContext(ctx, "controllers/HandleWebhook Skipping participant_left for non-local room: %s", roomName)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
				return
			}

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
			broadcastCallStop(ctx, roomName)

			// Best-effort: evict the room so the lingering transcription
			// agent disconnects promptly and LiveKit reclaims resources,
			// rather than holding the room open for its EmptyTimeout.
			business.DeleteRoom(ctx, roomName)
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// broadcastCallStop publishes the "call ended" MQTT signal to the correct
// audience based on the room-name shape. Shared by the room_finished and
// participant_left webhook paths.
//
// Room naming conventions (see roomBelongsToThisInstance):
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

// SaveMyTranscript POST /livekit/my-transcript — a participant persisting their
// OWN speech, from browser transcription mode.
//
// WHY THIS EXISTS. Browser mode is the DEFAULT, and it kept nothing: each
// browser transcribed locally for live captions and the words were discarded
// when the call ended. So a fresh install had no transcript, no meeting recap
// and no notes document until an admin found the setting and switched to the
// server-side agent. Everything downstream of a transcript was invisible to the
// people most likely to be evaluating the product.
//
// WHY IT IS SAFE. The agent's endpoint is server to server behind the internal
// secret; a browser cannot hold that. So this one is authenticated as the user
// and the identity is taken from the SESSION, never from the body: a caller can
// only ever attribute words to themselves. Presence in the room is checked
// against LiveKit, so they cannot post into a call they are not in, and the
// call-session key is resolved server-side rather than accepted, so they cannot
// choose which call their words land in.
//
// The worst a participant can do is put words they did not say into a call they
// are actually in, under their own name, which is what speaking already lets
// them do.
func SaveMyTranscript(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		RoomName string `json:"room_name"`
		Text     string `json:"text"`
		OffsetMs *int64 `json:"offset_ms,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}

	text := strings.TrimSpace(input.Text)
	if text == "" || strings.TrimSpace(input.RoomName) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "room_name and text are required"})
		return
	}
	// Same cap as the agent path: a transcript chunk is a sentence, not a file.
	if len(text) > 16*1024 {
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{"msg": "transcript text too long"})
		return
	}

	// Comma-ok, not a bare assertion. The router this lives on is mounted
	// unauthenticated for the webhook and agent endpoints, so a missing session
	// here must be a refusal rather than a panic on a reachable route.
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	if !ok || strings.TrimSpace(userInfo.UserDgraphInfo.Uid) == "" {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "sign in to save a transcript"})
		return
	}
	identity := userInfo.UserDgraphInfo.Uid

	sessionKey, err := business.ResolveTranscriptSession(ctx, input.RoomName, identity)
	if err != nil {
		// Not a participant, or no live call. Deliberately not a 500: this is a
		// normal answer to a request the server declines, and it is also what a
		// browser sees for a beat after everyone hangs up.
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "not in this call"})
		return
	}

	if err := business.SaveTranscript(ctx, &adapter.TranscriptInput{
		RoomName:            input.RoomName,
		ParticipantIdentity: identity,
		Text:                text,
		Timestamp:           time.Now().UnixMilli(),
		EgressID:            sessionKey,
		OffsetMs:            input.OffsetMs,
	}); err != nil {
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
// The client reports it because the client is the only thing that knows. The
// server can see that somebody spoke no lines, but cannot tell "said nothing"
// from "could not be heard", and guessing would caveat every meeting that had a
// quiet attendee in it.
//
// Same authentication shape as the transcript endpoint: the identity comes from
// the session and presence is checked against LiveKit, so a caller can only
// report about themselves, and only in a call they are actually in.
func ReportTranscriptionCapability(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		RoomName      string `json:"room_name"`
		CanTranscribe bool   `json:"can_transcribe"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.RoomName) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "room_name is required"})
		return
	}

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	if !ok || strings.TrimSpace(userInfo.UserDgraphInfo.Uid) == "" {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "sign in first"})
		return
	}
	identity := userInfo.UserDgraphInfo.Uid

	// Reuses the transcript grant: same question (are you in this call), same
	// cached answer, so a capability report costs no extra LiveKit calls.
	if _, err := business.ResolveTranscriptSession(ctx, input.RoomName, identity); err != nil {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "not in this call"})
		return
	}

	// Only the negative is recorded. "I can transcribe" is the assumption
	// everywhere else, so storing it would be a set of everyone in every call
	// to express the absence of a problem.
	if !input.CanTranscribe {
		if err := redisStore.AddToSet(ctx, registry.CallUntranscribed, []string{input.RoomName}, identity); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/ReportTranscriptionCapability could not record: %+v", err)
		}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "recorded"})
}

func SaveTranscript(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Authentication is enforced by the VerifyInternalServiceRequest
	// middleware mounted on this route in router.go: the LiveKit
	// transcription agent posts X-Internal-Secret matching the
	// server's INTERNAL_SECRET env var. Without that gate, anyone
	// who could reach the BE could attach arbitrary text to any
	// recording's transcript.

	var input adapter.TranscriptInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SaveTranscript Failed to decode transcript request: %v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}

	// Still required, but it is no longer only a recording's id: an unrecorded
	// call sends its session key here instead. What must never be accepted is an
	// EMPTY key, which would attach every unrecorded call in the workspace to
	// one shared node.
	if strings.TrimSpace(input.EgressID) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "egress_id or a call session key is required"})
		return
	}
	// Length caps so an abusive (or buggy) agent can't push oversized
	// payloads into Dgraph. Real transcripts are < 1 KB per chunk.
	if len(input.Text) > 16*1024 {
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{"msg": "transcript text too long"})
		return
	}

	err := business.SaveTranscript(ctx, &input)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save transcript"})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Transcript saved"})
}
