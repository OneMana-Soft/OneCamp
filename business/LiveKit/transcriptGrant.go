package business

// Browser-mode transcription posts its own words, so the server has to decide
// whether to believe the caller.
//
// The agent posts transcripts over the internal secret, which is server to
// server and trusted. A browser cannot hold that secret, so a participant
// posting their own speech is authenticated as themselves and checked here: are
// they in the room they claim, and which call is it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	livekitInit "github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// ResolveTranscriptSession returns the call-session key a participant's
// transcript lines belong under, after confirming they are in the room.
//
// Returns an error when the caller is not in the room, which the controller
// maps to a refusal. A room with no live call is the same answer: there is
// nothing to transcribe into.
//
// Cached per (room, participant) for the grant's TTL, because this runs once
// per utterance. The cache holds only a decision the server already made, so a
// cold cache costs correctness nothing and a warm one costs LiveKit nothing.
func ResolveTranscriptSession(ctx context.Context, roomName, identity string) (string, error) {
	roomName = strings.TrimSpace(roomName)
	identity = strings.TrimSpace(identity)
	if roomName == "" || identity == "" {
		return "", fmt.Errorf("room and participant are required")
	}

	if key, ok, err := redisStore.GetString(ctx, registry.CallTranscriptGrant, []string{roomName, identity}); err == nil && ok && key != "" {
		return key, nil
	}

	sid, present, err := roomSessionForParticipant(ctx, roomName, identity)
	if err != nil {
		return "", err
	}
	if !present {
		return "", fmt.Errorf("not a participant of this call")
	}
	if sid == "" {
		return "", fmt.Errorf("call has no session")
	}

	key := helpers.CallSessionKeyFor(sid)
	// Best-effort: a cache write failing costs a LiveKit call next time, not
	// correctness.
	_ = redisStore.SetString(ctx, registry.CallTranscriptGrant, []string{roomName, identity}, key)
	return key, nil
}

// roomSessionForParticipant asks LiveKit for the room's session id and whether
// this identity is connected to it.
func roomSessionForParticipant(ctx context.Context, roomName, identity string) (sid string, present bool, err error) {
	rooms, err := livekitInit.LiveKitService.ListRooms(ctx, roomName)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/roomSessionForParticipant list rooms err: %+v", err)
		return "", false, err
	}
	for _, r := range rooms {
		if r != nil && r.Name == roomName {
			sid = r.Sid
			break
		}
	}
	if sid == "" {
		return "", false, nil
	}

	present, err = livekitInit.LiveKitService.RoomHasParticipant(ctx, roomName, identity)
	if err != nil {
		return "", false, err
	}
	return sid, present, nil
}
