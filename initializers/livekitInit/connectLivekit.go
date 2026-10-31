package livekitInit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
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

// CallPublishSources is everything someone in a call may publish: their own
// microphone and camera, and their screen. What the people running the call
// hold for everyone else narrows it (business/LiveKit's PublishSources).
//
// It used to follow the green room's toggles. Someone who joined muted got a
// token without "microphone", so Unmute was refused by the server ("failed to
// publish track, insufficient permissions") for the rest of the call, and the
// same for the camera. A token says what a person is ALLOWED to do in the
// call, not what they chose to do at the door: the toggles are the client's
// state.
var CallPublishSources = []string{"camera", "microphone", "screen_share", "screen_share_audio"}

// CallTokenTTL is how long a minted call token can be used to connect.
//
// Short on purpose. LiveKit refreshes the token of a CONNECTED participant
// itself (every five minutes, each refresh valid for ten), so this bounds only
// the window to connect and how long a token outlives the person's right to
// it: removed from the channel, a guest link revoked, put out of the call.
// It was 24 hours for members and 4 for guests. Clients mint at Join and mint
// again from the same endpoint after a full disconnect.
const CallTokenTTL = 10 * time.Minute

// LanguageAttribute is the participant attribute that carries the language a
// person speaks (from their browser), so the server's transcriber can use it.
const LanguageAttribute = "oc.lang"

// languageTag is the shape of a BCP-47 tag as browsers report it ("en",
// "en-US", "zh-Hant-TW"), and nothing else: the value is copied into a token
// and on to a speech provider.
var languageTag = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8}){0,4}$`)

// CleanLanguage returns a usable language tag, or "" for anything that is not
// one. Pure.
func CleanLanguage(raw string) string {
	tag := strings.TrimSpace(raw)
	if len(tag) > 35 || !languageTag.MatchString(tag) {
		return ""
	}
	return tag
}

// callGrant is the grant a call token carries, and, as a permission, what
// SetPublishSources gives someone already in the call: sources is what they
// may publish (some of CallPublishSources). Pure.
func callGrant(room string, roomAdmin bool, sources []string) *lka.VideoGrant {
	grant := &lka.VideoGrant{
		RoomJoin:  true,
		Room:      room,
		RoomAdmin: roomAdmin,
	}
	// LiveKit reads an empty list as every source, so none is no publishing.
	grant.SetCanPublish(len(sources) > 0)
	grant.SetCanSubscribe(true)
	grant.SetCanPublishData(true)
	grant.CanPublishSources = append([]string(nil), sources...)
	return grant
}

// mintCallToken signs a token for one room, with the person's language when
// they gave a usable one.
func (s *LiveKitServiceStruct) mintCallToken(room, identity, name string, roomAdmin bool, language string, sources []string) (string, error) {
	at := lka.NewAccessToken(s.Config.ApiKey, s.Config.ApiSecret)
	at.SetVideoGrant(callGrant(room, roomAdmin, sources)).
		SetIdentity(identity).
		SetName(name).
		SetValidFor(CallTokenTTL)
	if lang := CleanLanguage(language); lang != "" {
		at.SetAttributes(map[string]string{LanguageAttribute: lang})
	}
	return at.ToJWT()
}

// GenerateToken mints a member's token for a call room, letting them publish
// sources (some of CallPublishSources). RoomAdmin only for the host of an
// instant meeting.
func (s *LiveKitServiceStruct) GenerateToken(room, identity, name string, isAdmin bool, language string, sources []string) (string, error) {
	return s.mintCallToken(room, identity, name, isAdmin, language, sources)
}

// GenerateGuestToken mints a single-room LiveKit token for an unauthenticated
// guest. Unlike GenerateToken it NEVER grants RoomAdmin and is scoped to exactly
// one room. The identity is supplied by the caller in the reserved `guest-`
// namespace so it can never collide with a member's identity or the
// transcription agent. sources is what they may publish (some of
// CallPublishSources).
func (s *LiveKitServiceStruct) GenerateGuestToken(room, identity, name, language string, sources []string) (string, error) {
	return s.mintCallToken(room, identity, name, false, language, sources)
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
	if _, err = s.UpdateRoomMetadata(ctx, roomName, recordingKeys(true, userName, "")); err != nil {
		helpers.LogErrorWithContext(ctx, "liveKitInit/StartRecording could not update the metadata of %s: %v", roomName, err)
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
	if _, err = s.UpdateRoomMetadata(ctx, roomName, recordingKeys(true, userName, egressInfo.EgressId)); err != nil {
		// Just log error, don't fail the request as recording started successfully
		helpers.LogErrorWithContext(ctx, "liveKitInit/StartRecording could not add the egress to the metadata of %s: %v", roomName, err)
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

	// Update metadata. Only the recording's keys: this wrote the whole
	// metadata as {"isRecording": false}, which also switched off everything
	// else a call keeps there (the transcription switch and its notice).
	if _, err = s.UpdateRoomMetadata(ctx, roomName, recordingKeys(false, "", "")); err != nil {
		helpers.LogErrorWithContext(ctx, "liveKitInit/StopRecording could not update the metadata of %s: %v", roomName, err)
	}

	return lastEgress, nil
}

// Ready reports whether calls are configured on this server at all. They are
// an optional subsystem: a server without LiveKit has no clients.
func (s *LiveKitServiceStruct) Ready() bool {
	return s != nil && s.LiveKitClient != nil && s.Config != nil
}

// ErrRoomNotFound is a call room that does not exist (nobody is in a call
// there, or it has just ended).
var ErrRoomNotFound = errors.New("no such call room")

// roomMetadataLocks serialises metadata edits per room in this process.
var roomMetadataLocks helpers.KeyedLock

// UpdateRoomMetadata changes some keys of a room's metadata and keeps the rest.
//
// The metadata is one JSON object that several features write: the recording
// state, and the transcription switch with its notice. Each write used to
// replace the whole object, so stopping a recording erased the switch. Keys in
// set are written, and a nil value removes its key. Edits to one room run one
// at a time here, so two features changing it at once can't lose each other's
// keys. Answers the metadata written.
func (s *LiveKitServiceStruct) UpdateRoomMetadata(ctx context.Context, room string, set map[string]any) (string, error) {
	roomMetadataLocks.Acquire(room, 0)
	defer roomMetadataLocks.Release(room)

	current, err := s.GetRoom(ctx, room)
	if err != nil {
		return "", err
	}
	next, err := mergeRoomMetadata(current.Metadata, set)
	if err != nil {
		return "", err
	}
	if _, err = s.LiveKitClient.UpdateRoomMetadata(ctx, &livekit.UpdateRoomMetadataRequest{
		Room:     room,
		Metadata: next,
	}); err != nil {
		return "", err
	}
	return next, nil
}

// mergeRoomMetadata writes set's keys into a room's metadata, keeping every
// other key. Metadata that is empty or not a JSON object counts as no keys,
// rather than as a reason to refuse: it is what an older server wrote. Pure.
func mergeRoomMetadata(current string, set map[string]any) (string, error) {
	keys := map[string]json.RawMessage{}
	if strings.TrimSpace(current) != "" {
		if err := json.Unmarshal([]byte(current), &keys); err != nil {
			keys = map[string]json.RawMessage{}
		}
	}
	for k, v := range set {
		if v == nil {
			delete(keys, k)
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		keys[k] = raw
	}
	out, err := json.Marshal(keys)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// GetRoom answers the room of that name, or ErrRoomNotFound.
func (s *LiveKitServiceStruct) GetRoom(ctx context.Context, room string) (*livekit.Room, error) {
	rooms, err := s.ListRooms(ctx, room)
	if err != nil {
		return nil, err
	}
	for _, r := range rooms {
		if r != nil && r.Name == room {
			return r, nil
		}
	}
	return nil, ErrRoomNotFound
}

// ConnectedParticipants answers who is connected to a room now: people, and
// whoever isn't one (the transcription agent, a recorder, a hidden watcher),
// which callers tell apart with IsPerson. Someone LiveKit still lists as
// DISCONNECTED has left. An unknown room has nobody.
func (s *LiveKitServiceStruct) ConnectedParticipants(ctx context.Context, room string) ([]*livekit.ParticipantInfo, error) {
	resp, err := s.LiveKitClient.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		if isRoomNotFoundErr(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*livekit.ParticipantInfo, 0, len(resp.Participants))
	for _, p := range resp.Participants {
		if p == nil || p.State == livekit.ParticipantInfo_DISCONNECTED {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// SendData sends a data packet from the server (no sending participant, which
// is how a client tells it apart from anything a participant could send) to
// the given identities, or to everyone in the room when there are none.
func (s *LiveKitServiceStruct) SendData(ctx context.Context, room, topic string, payload []byte, identities ...string) error {
	t := topic
	_, err := s.LiveKitClient.SendData(ctx, &livekit.SendDataRequest{
		Room:                  room,
		Data:                  payload,
		Kind:                  livekit.DataPacket_RELIABLE,
		Topic:                 &t,
		DestinationIdentities: identities,
	})
	return err
}

// RemoveParticipant puts someone out of a room now. Someone already gone is
// not an error: the point, "not in the call", holds.
func (s *LiveKitServiceStruct) RemoveParticipant(ctx context.Context, room, identity string) error {
	_, err := s.LiveKitClient.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     room,
		Identity: identity,
	})
	if err != nil && isRoomNotFoundErr(err) {
		return nil
	}
	return err
}

// ListRooms lists those of the rooms named that exist. No names, no rooms:
// LiveKit would list every room (ListAllRooms).
func (s *LiveKitServiceStruct) ListRooms(ctx context.Context, names ...string) (rooms []*livekit.Room, err error) {
	if len(names) == 0 {
		return nil, nil
	}
	resp, err := s.LiveKitClient.ListRooms(ctx, &livekit.ListRoomsRequest{
		Names: names,
	})

	if err != nil {
		return
	}

	rooms = resp.Rooms
	return
}

func (s *LiveKitServiceStruct) ListAllRooms(ctx context.Context) (rooms []*livekit.Room, err error) {
	// Calls not set up (no LiveKit): no rooms, said as an error the callers
	// already handle, never a nil dereference that fails the whole request
	// (the sidebar, and /v1/unread with it).
	if s.LiveKitClient == nil {
		return nil, errors.New("livekit isn't configured")
	}
	resp, err := s.LiveKitClient.ListRooms(ctx, &livekit.ListRoomsRequest{})

	if err != nil {
		return
	}

	rooms = resp.Rooms
	return
}

// RoomHasHumanParticipants reports whether a person (IsPerson) is connected to
// the named room now: the room's part of "is a call active".
//
// It deliberately does NOT use room existence (ListRooms), nor count anyone
// connected: the transcription agent auto-joins every room, a recording's
// egress stays in a room nobody is in, a hidden watcher is nobody's call, and
// LiveKit keeps an empty room object alive for EmptyTimeout seconds after the
// last participant leaves. Each would report a call as "active" after every
// person has hung up.
//
// An unknown room has nobody in it: (false, nil), so callers don't surface a
// spurious failure on the common "no call" path.
func (s *LiveKitServiceStruct) RoomHasHumanParticipants(ctx context.Context, roomName string) (bool, error) {
	people, err := s.ConnectedParticipants(ctx, roomName)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(people, IsPerson), nil
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
	people, err := s.ConnectedParticipants(ctx, roomName)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(people, func(p *livekit.ParticipantInfo) bool { return p.Identity == identity }), nil
}

// HumanActiveRoomNames returns the set of room names that currently have at
// least one human (non-agent) participant. Used by the sidebar hydration
// path to compute call-active state for many conversations in one pass
// without an N+1 ListParticipants call per conversation.
//
// It lists all rooms, then asks each one whether a person is in it (personIn).
// A room with nobody but the agent, a recorder or a hidden watcher in it is
// left out.
func (s *LiveKitServiceStruct) HumanActiveRoomNames(ctx context.Context) (map[string]bool, error) {
	// No calls server connected: no call is active. The sidebar asks on every
	// read, and a nil client took the whole read down with it.
	if s.LiveKitClient == nil {
		return nil, errors.New("livekit isn't connected")
	}
	rooms, err := s.ListAllRooms(ctx)
	if err != nil {
		return nil, err
	}
	active := make(map[string]bool, len(rooms))
	for _, room := range rooms {
		if s.personIn(ctx, room) {
			active[room.Name] = true
		}
	}
	return active, nil
}

// RoomsHaveHumanParticipants reports whether a person is in any of the rooms
// named. It lists them at once, then asks who is in them only until a person
// turns up (personIn). No names, no person. The call's dot asks it of a call's
// breakout rooms; the sidebar, which wants every room with a person in it,
// reads HumanActiveRoomNames.
func (s *LiveKitServiceStruct) RoomsHaveHumanParticipants(ctx context.Context, names ...string) (bool, error) {
	rooms, err := s.ListRooms(ctx, names...)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(rooms, func(room *livekit.Room) bool { return s.personIn(ctx, room) }), nil
}

// personIn is the one per-room answer to "is a person in this room"
// (RoomHasHumanParticipants), asked only where LiveKit counts someone: a room
// with nobody connected can't have a person. A room that can't be read counts
// as having nobody in it this pass, so one room never fails a whole read.
func (s *LiveKitServiceStruct) personIn(ctx context.Context, room *livekit.Room) bool {
	if room.NumParticipants == 0 {
		return false
	}
	hasHuman, err := s.RoomHasHumanParticipants(ctx, room.Name)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"liveKitInit/personIn Failed to list participants for room %s err: %+v",
			room.Name, err)
		return false
	}
	return hasHuman
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

// IsPerson is the one rule for whether someone connected to a call is a person
// in it: not the transcription agent, a recorder (an egress, which LiveKit
// connects hidden) or a hidden watcher. Only a person keeps a call going and
// lights its dot, so every "is anyone in this call" asks this: the paths that
// end a call (business/CallControl), the call's dot and the sidebar's
// (RoomHasHumanParticipants), and who a call's view lists. Pure.
func IsPerson(p *livekit.ParticipantInfo) bool {
	if p == nil || strings.TrimSpace(p.Identity) == "" || isAgentIdentity(p.Identity) {
		return false
	}
	if p.Permission != nil && p.Permission.Hidden {
		return false
	}
	return p.Kind != livekit.ParticipantInfo_EGRESS && p.Kind != livekit.ParticipantInfo_AGENT
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

// How long a call room waits for people.
//
// The room is made when the first token is minted, before anyone connects. It
// closed after 5 seconds with nobody in it (measured on v1.11.0: there at 4 s,
// gone at 6 s), so a first person whose browser took longer to connect (a
// phone, a slow network, a TURN relay) had the call end before it began: the
// call-stopped broadcast, the recap, the "meeting ended" workflows, and then a
// fresh room with no live dot when they did connect.
const (
	// roomJoinGrace is how long a room with nobody in it waits for its first
	// person, and for the last to come back (LiveKit's empty timeout).
	roomJoinGrace = 120
	// roomDepartureGrace keeps a room for a moment after its last person leaves,
	// so a reconnect after a dropped network is the same call.
	roomDepartureGrace = 20
)

func (s *LiveKitServiceStruct) CreateRoom(ctx context.Context, roomName string) (lr *livekit.Room, err error) {
	lr, err = s.LiveKitClient.CreateRoom(ctx, callRoomRequest(roomName))
	return
}

// callRoomRequest is how a call room is made. Pure.
func callRoomRequest(roomName string) *livekit.CreateRoomRequest {
	return &livekit.CreateRoomRequest{
		Name:             roomName,
		MaxParticipants:  100,
		EmptyTimeout:     roomJoinGrace,
		DepartureTimeout: roomDepartureGrace,
	}
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

// recordingKeys are the room metadata's recording keys: whether a recording
// runs, who started it (by the name people see) and its egress once there is
// one. Stopping removes the last two. Values, not text pasted into a string,
// so a name with a quote or a backslash in it can't break the metadata every
// client parses. Pure.
func recordingKeys(on bool, startedBy, egressID string) map[string]any {
	keys := map[string]any{"isRecording": on, "recordingStartedBy": nil, "egressID": nil}
	if on {
		keys["recordingStartedBy"] = startedBy
		if egressID != "" {
			keys["egressID"] = egressID
		}
	}
	return keys
}
