package livekitInit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	lka "github.com/livekit/protocol/auth"
	livekit "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

type MinIoConfigStruct struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	BucketName      string
}

type LiveKitConfigStruct struct {
	HostURL     string
	ApiKey      string
	ApiSecret   string
	MinIoConfig MinIoConfigStruct
}

type LiveKitServiceStruct struct {
	LiveKitClient *lksdk.RoomServiceClient
	EgressClient  *lksdk.EgressClient
	Config        *LiveKitConfigStruct
}

var LiveKitService LiveKitServiceStruct

// transcriberAgentIdentity is the participant identity used by the
// transcription agent (livekit-agent/agent.py → AGENT_IDENTITY). The
// agent auto-joins every call room as a participant, so a room with ONLY
// the agent connected is NOT a human-active call. Call-active detection
// must ignore this identity, otherwise the "call active" dot lingers (or
// never clears) for as long as the agent sits in an otherwise-empty room.
//
// Keep this in sync with AGENT_IDENTITY in livekit-agent/agent.py.
const transcriberAgentIdentity = "transcriber-bot"

// isAgentIdentity reports whether a participant identity belongs to a
// non-human service participant (currently just the transcriber bot).
// Centralised so future service participants (e.g. a recording bot) can
// be excluded from human-active-call detection in one place.
func isAgentIdentity(identity string) bool {
	return identity == transcriberAgentIdentity
}

func ConnectLiveKit(config *LiveKitConfigStruct) (err error) {

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	LiveKitService.Config = config
	LiveKitService.LiveKitClient = lksdk.NewRoomServiceClient(config.HostURL, config.ApiKey, config.ApiSecret)
	LiveKitService.EgressClient = lksdk.NewEgressClient(config.HostURL, config.ApiKey, config.ApiSecret)

	_, err = LiveKitService.LiveKitClient.ListRooms(ctx, &livekit.ListRoomsRequest{})

	if err != nil {
		helpers.LogErrorWithContext(ctx, "liveKitInit/ConnectLiveKit Failed to connect room service to livekit")
		noteReachability(false)
		return
	}

	_, err = LiveKitService.EgressClient.ListEgress(ctx, &livekit.ListEgressRequest{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "liveKitInit/ConnectLiveKit: Failed to connect to LiveKit Egress service")
		noteReachability(false)
		return err
	}

	helpers.MessageLogs.InfoLog.Println("Successfully connected with livekit room service client and egress client!")

	// Seed the availability cache so the first /config/client request answers from it
	// rather than repeating a probe we have just done.
	noteReachability(true)

	return

}

func (s *LiveKitServiceStruct) GenerateToken(room, identity, name string, isAdmin bool, audioEnabled bool, videoEnabled bool) (string, error) {
	at := lka.NewAccessToken(s.Config.ApiKey, s.Config.ApiSecret)

	grant := &lka.VideoGrant{
		RoomJoin:     true,
		Room:         room,
		CanPublish:   &[]bool{true}[0],
		CanSubscribe: &[]bool{true}[0],
	}

	if isAdmin {
		grant.RoomAdmin = []bool{true}[0]
	}

	sources := []string{"screen_share", "screen_share_audio"}

	if audioEnabled {
		sources = append(sources, "microphone")
	}

	if videoEnabled {
		sources = append(sources, "camera")
	}

	if len(sources) > 0 {
		grant.CanPublishSources = sources
	} else {
		grant.CanPublishSources = nil
	}

	at.SetVideoGrant(grant).
		SetIdentity(identity).
		SetName(name).
		SetValidFor(24 * time.Hour)

	return at.ToJWT()
}

// GenerateGuestToken mints a single-room LiveKit token for an unauthenticated
// guest. Unlike GenerateToken it NEVER grants RoomAdmin, is scoped to exactly
// one room, and uses a short, caller-supplied validity window. The identity is
// supplied by the caller in the reserved `guest-` namespace so it can never
// collide with a member's UUID identity or the transcription agent.
func (s *LiveKitServiceStruct) GenerateGuestToken(room, identity, name string, ttl time.Duration, audioEnabled, videoEnabled bool) (string, error) {
	at := lka.NewAccessToken(s.Config.ApiKey, s.Config.ApiSecret)

	grant := &lka.VideoGrant{
		RoomJoin:     true,
		Room:         room,
		CanPublish:   &[]bool{true}[0],
		CanSubscribe: &[]bool{true}[0],
	}

	sources := []string{"screen_share", "screen_share_audio"}
	if audioEnabled {
		sources = append(sources, "microphone")
	}
	if videoEnabled {
		sources = append(sources, "camera")
	}
	grant.CanPublishSources = sources

	at.SetVideoGrant(grant).
		SetIdentity(identity).
		SetName(name).
		SetValidFor(ttl)

	return at.ToJWT()
}

func (s *LiveKitServiceStruct) StartRecording(ctx context.Context, roomName string, userName string) (*livekit.EgressInfo, string, error) {

	// 1. Check if room exists and has participants
	// We use a retry loop because LiveKit might take a few seconds to register
	// the room or the first participant after they join.
	var rooms *livekit.ListRoomsResponse
	var err error
	for i := 0; i < 5; i++ {
		rooms, err = s.LiveKitClient.ListRooms(ctx, &livekit.ListRoomsRequest{
			Names: []string{roomName},
		})
		if err == nil && len(rooms.Rooms) > 0 {
			// Room exists, we can proceed. Egress will wait for participants if needed.
			break
		}
		// helpers.LogInfoWithContext(ctx, fmt.Sprintf("liveKitInit/StartRecording: Room %s not ready, retrying... (attempt %d/5)", roomName, i+1))
		time.Sleep(1 * time.Second)
	}

	if err != nil {
		return nil, "", fmt.Errorf("StartRecording failed to list rooms : %+v", err)
	}

	if len(rooms.Rooms) == 0 {
		return nil, "", fmt.Errorf("room %s does not exist", roomName)
	}

	// 2. Check if already recording
	egressList, err := s.EgressClient.ListEgress(ctx, &livekit.ListEgressRequest{
		RoomName: roomName,
		Active:   true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to check active recordings: %w", err)
	}

	if len(egressList.Items) > 0 {
		return nil, "", fmt.Errorf("recording already in progress for room %s", roomName)
	}

	// 3. Start Recording
	// Initial metadata before we have the ID
	metadata := "{\"isRecording\": true, \"recordingStartedBy\": \"" + userName + "\"}"

	_, err = s.LiveKitClient.UpdateRoomMetadata(ctx, &livekit.UpdateRoomMetadataRequest{
		Room:     roomName,
		Metadata: metadata,
	})

	if err != nil {
		fmt.Printf("Error updating room metadata for %s: %v\n", roomName, err)
		return nil, "", fmt.Errorf("failed to update room metadata: %w", err)
	}

	filename := fmt.Sprintf("recordings/%s/%s.mp4", roomName, time.Now().Format(time.RFC3339))

	egressInfo, err := s.EgressClient.StartRoomCompositeEgress(ctx, &livekit.RoomCompositeEgressRequest{
		RoomName: roomName,
		Layout:   "grid-light",
		Options: &livekit.RoomCompositeEgressRequest_Preset{
			Preset: EgressPreset(os.Getenv("EGRESS_PRESET")),
		},
		FileOutputs: []*livekit.EncodedFileOutput{
			{
				Filepath: filename,
				Output: &livekit.EncodedFileOutput_S3{
					S3: &livekit.S3Upload{
						AccessKey:      s.Config.MinIoConfig.AccessKeyID,
						Secret:         s.Config.MinIoConfig.SecretAccessKey,
						Bucket:         s.Config.MinIoConfig.BucketName,
						Endpoint:       s.Config.MinIoConfig.Endpoint,
						ForcePathStyle: true,
					},
				},
			},
		},
	})

	if err != nil {
		return nil, filename, err
	}

	// 4. Update Metadata with EgressID
	// We append the egressId to the metadata so agents can find it
	metadataWithID := fmt.Sprintf("{\"isRecording\": true, \"recordingStartedBy\": \"%s\", \"egressID\": \"%s\"}", userName, egressInfo.EgressId)
	_, err = s.LiveKitClient.UpdateRoomMetadata(ctx, &livekit.UpdateRoomMetadataRequest{
		Room:     roomName,
		Metadata: metadataWithID,
	})
	if err != nil {
		// Just log error, don't fail the request as recording started successfully
		fmt.Printf("Error updating room metadata with EgressID for %s: %v\n", roomName, err)
	}

	return egressInfo, filename, nil
}

func (s *LiveKitServiceStruct) StopRecording(ctx context.Context, roomName string) (lastEgress *livekit.EgressInfo, err error) {
	// Find active egress for the room
	egressList, err := s.EgressClient.ListEgress(ctx, &livekit.ListEgressRequest{
		RoomName: roomName,
		Active:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list egress for room: %w", err)
	}

	if len(egressList.Items) == 0 {
		return nil, fmt.Errorf("no active recording found for room %s", roomName)
	}

	var lastErr error

	// Stop all active egresses for this room (should usually be just one)
	for _, egress := range egressList.Items {
		stopReq := &livekit.StopEgressRequest{
			EgressId: egress.EgressId,
		}
		info, err := s.EgressClient.StopEgress(ctx, stopReq)
		if err != nil {
			lastErr = err
		} else {
			lastEgress = info
		}
	}

	if lastErr != nil && lastEgress == nil {
		return nil, lastErr
	}

	// Update metadata
	_, err = s.LiveKitClient.UpdateRoomMetadata(ctx, &livekit.UpdateRoomMetadataRequest{
		Room:     roomName,
		Metadata: "{\"isRecording\": false}",
	})
	if err != nil {
		fmt.Printf("Error updating room metadata for %s: %v\n", roomName, err)
	}

	return lastEgress, nil
}

func (s *LiveKitServiceStruct) ListRooms(ctx context.Context, roomName string) (rooms []*livekit.Room, err error) {
	resp, err := s.LiveKitClient.ListRooms(ctx, &livekit.ListRoomsRequest{
		Names: []string{roomName},
	})

	if err != nil {
		return
	}

	rooms = resp.Rooms
	return
}

func (s *LiveKitServiceStruct) ListAllRooms(ctx context.Context) (rooms []*livekit.Room, err error) {
	resp, err := s.LiveKitClient.ListRooms(ctx, &livekit.ListRoomsRequest{})

	if err != nil {
		return
	}

	rooms = resp.Rooms
	return
}

// RoomHasHumanParticipants reports whether the named room currently has at
// least one human (non-agent) participant connected.
//
// This is the authoritative "is a call active" check. It deliberately does
// NOT use room existence (ListRooms), because:
//   - The transcription agent auto-joins every room, so the room object
//     outlives the human call until the agent's own empty-room timeout.
//   - LiveKit keeps an empty room object alive for EmptyTimeout seconds
//     after the last participant leaves.
//
// Both cases would otherwise report a call as "active" after every human
// has hung up. By counting non-agent participants we report inactive the
// instant the last human leaves.
//
// Returns (false, nil) when the room does not exist — LiveKit's
// ListParticipants returns NotFound for an unknown room, which we treat as
// "no active call" rather than an error so callers don't surface spurious
// failures on the common "no call" path.
func (s *LiveKitServiceStruct) RoomHasHumanParticipants(ctx context.Context, roomName string) (active bool, err error) {
	return s.roomHasHumanParticipantsExcluding(ctx, roomName, "")
}

// RoomHasHumanParticipantsExcluding is like RoomHasHumanParticipants but
// ignores the participant with identity excludeIdentity.
//
// Used by the participant_left webhook: depending on LiveKit's internal
// ordering, the just-departed participant may still appear in
// ListParticipants for a brief window. Excluding them makes the
// "is anyone still here?" decision correct regardless of that race, so we
// don't miss broadcasting the call-stop when the last human leaves.
func (s *LiveKitServiceStruct) RoomHasHumanParticipantsExcluding(ctx context.Context, roomName, excludeIdentity string) (active bool, err error) {
	return s.roomHasHumanParticipantsExcluding(ctx, roomName, excludeIdentity)
}

// RoomHasParticipant reports whether one identity is currently connected to a
// room.
//
// Distinct from the "is a call active" checks above, which ask about anyone.
// This asks about a specific person, because browser-mode transcription needs
// to know that the caller posting words is in the call those words belong to.
//
// A DISCONNECTED entry does not count: LiveKit lists a participant briefly
// after they leave, and accepting that would extend the window in which someone
// who has left can still post.
//
// An unknown room is "not present" rather than an error, matching how the other
// checks here treat it.
func (s *LiveKitServiceStruct) RoomHasParticipant(ctx context.Context, roomName, identity string) (bool, error) {
	resp, err := s.LiveKitClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{
		Room: roomName,
	})
	if err != nil {
		if isRoomNotFoundErr(err) {
			return false, nil
		}
		return false, err
	}
	for _, p := range resp.Participants {
		if p.State == livekit.ParticipantInfo_DISCONNECTED {
			continue
		}
		if p.Identity == identity {
			return true, nil
		}
	}
	return false, nil
}

func (s *LiveKitServiceStruct) roomHasHumanParticipantsExcluding(ctx context.Context, roomName, excludeIdentity string) (active bool, err error) {
	resp, err := s.LiveKitClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{
		Room: roomName,
	})
	if err != nil {
		// Unknown room → no active call. LiveKit returns a NotFound-style
		// error for a room that doesn't exist; treat that as inactive.
		if isRoomNotFoundErr(err) {
			return false, nil
		}
		return false, err
	}

	for _, p := range resp.Participants {
		// Only ACTIVE/JOINED participants count. A participant that is
		// JOINING or DISCONNECTED is not in a live call.
		if p.State == livekit.ParticipantInfo_DISCONNECTED {
			continue
		}
		if isAgentIdentity(p.Identity) {
			continue
		}
		if excludeIdentity != "" && p.Identity == excludeIdentity {
			continue
		}
		return true, nil
	}
	return false, nil
}

// HumanActiveRoomNames returns the set of room names that currently have at
// least one human (non-agent) participant. Used by the sidebar hydration
// path to compute call-active state for many conversations in one pass
// without an N+1 ListParticipants call per conversation.
//
// It lists all rooms, then queries participants only for rooms that report
// NumParticipants > 0 (rooms with zero participants can't have a human).
// Rooms whose only participant is the agent are excluded from the result.
func (s *LiveKitServiceStruct) HumanActiveRoomNames(ctx context.Context) (map[string]bool, error) {
	rooms, err := s.ListAllRooms(ctx)
	if err != nil {
		return nil, err
	}

	active := make(map[string]bool, len(rooms))
	for _, room := range rooms {
		if room.NumParticipants == 0 {
			continue
		}
		hasHuman, perr := s.RoomHasHumanParticipants(ctx, room.Name)
		if perr != nil {
			// Don't fail the whole batch for one room; log-and-skip keeps
			// the sidebar resilient. A transient per-room error simply
			// means that room is treated as inactive this pass.
			helpers.LogErrorWithContext(ctx,
				"liveKitInit/HumanActiveRoomNames Failed to list participants for room %s err: %+v",
				room.Name, perr)
			continue
		}
		if hasHuman {
			active[room.Name] = true
		}
	}
	return active, nil
}

// isRoomNotFoundErr reports whether a LiveKit API error indicates the room
// does not exist. LiveKit surfaces this as a twirp error; we match on the
// message defensively so a version bump in error wording still degrades to
// "no active call" rather than a hard error.
func isRoomNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "notfound") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such room")
}

// IsAgentIdentity reports whether a participant identity belongs to a
// non-human service participant (the transcription agent). Exported so
// webhook handlers can skip the agent's own join/leave events.
func IsAgentIdentity(identity string) bool {
	return isAgentIdentity(identity)
}

// DeleteRoom force-closes a LiveKit room, disconnecting all remaining
// participants (including the transcription agent). Best-effort: a
// not-found error is treated as success since the goal — "room gone" — is
// already satisfied.
func (s *LiveKitServiceStruct) DeleteRoom(ctx context.Context, roomName string) error {
	_, err := s.LiveKitClient.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
		Room: roomName,
	})
	if err != nil && !isRoomNotFoundErr(err) {
		return err
	}
	return nil
}

func (s *LiveKitServiceStruct) CreateRoom(ctx context.Context, roomName string) (lr *livekit.Room, err error) {
	roomReq := &livekit.CreateRoomRequest{
		Name:             roomName,
		MaxParticipants:  100,
		EmptyTimeout:     5,
		DepartureTimeout: 0,
	}

	lr, err = s.LiveKitClient.CreateRoom(ctx, roomReq)

	return
}

// EgressPreset is the recording quality, from EGRESS_PRESET. Recording a call
// is the heaviest single thing a machine does, and 1080p on a four-core box
// starves everything else while it runs; `make tune` sets 720p30 there. An
// unknown or empty value is 1080p30, which is what every install had before
// the variable existed.
func EgressPreset(name string) livekit.EncodingOptionsPreset {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "720p30", "720p":
		return livekit.EncodingOptionsPreset_H264_720P_30
	case "720p60":
		return livekit.EncodingOptionsPreset_H264_720P_60
	case "1080p60":
		return livekit.EncodingOptionsPreset_H264_1080P_60
	}
	return livekit.EncodingOptionsPreset_H264_1080P_30
}
