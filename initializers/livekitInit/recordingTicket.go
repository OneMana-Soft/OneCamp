package livekitInit

// The recording stage's ticket (H3, meet-17). The web's layout page draws a
// live call, so LiveKit's recorder reaches it only through the API's recording
// stage (RecordingStagePath), holding a ticket minted for the one recording it
// is starting.
//
// Permission: the ticket is the only way in. It is sealed under the server's
// secret for this purpose alone (helpers.Seal), names its room, and is good
// for five minutes (registry.RecordingStageTicket's TTL) and once: the room keeps its latest recording's ticket
// in Redis, a claim takes it there in one step (DeleteIfHeld), a later start
// in the room replaces it (a grid start drops it) and the recording's end
// revokes it. It goes only into the egress request this server sends LiveKit,
// never into room metadata, a recording's row, a member's response or a log.
// The stage then hands the recorder a token of its own (RecorderToken): its
// room alone, hidden, subscribing alone, as an egress, for RecorderTokenTTL.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	authService "github.com/akashc777/OneCamp/services/Auth"
	lka "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/server-sdk-go/v2/signalling"
)

// RecordingStagePath is the API's address the recorder opens, its ticket
// next: a path, so egress's own query (layout, url, token) can't touch it.
const RecordingStagePath = "/recording-stage/"

const (
	// RecorderTokenTTL is how long the recorder's token may be used to
	// connect: the page connects as soon as it loads, and stays connected.
	RecorderTokenTTL = 2 * time.Minute
	// stagePurpose names the key tickets are sealed under (helpers.Seal).
	stagePurpose = "onecamp.recording-stage.v1"
)

// stageTicket is what a ticket carries: its room, the nonce the room keeps
// for its latest recording, and when it stops.
type stageTicket struct {
	Room  string `json:"r"`
	Nonce string `json:"n"`
	Exp   int64  `json:"e"`
}

// stageEntranceURL mints the ticket of the recording about to start in room
// and answers the address the recorder opens with it: the API's recording
// stage. "" when the stage can't be drawn, and the recording draws LiveKit's
// grid: the web has no origin the recorder can reach (RecordingLayoutURL), nor
// the API (none set, or this machine's own), or a ticket can't be kept (no
// Redis, no secret).
func stageEntranceURL(ctx context.Context, room string) string {
	api := authService.BackendBaseURL()
	if RecordingLayoutURL() == "" || authService.LocalHost(api) || !redisStore.IsAvailable() {
		return ""
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	// A ticket waits for its recorder, which opens it within seconds of the
	// start, as long as the room keeps it.
	life := registry.RecordingStageTicket.TTL
	t := stageTicket{Room: room, Nonce: hex.EncodeToString(nonce), Exp: time.Now().Add(life).Unix()}
	ticket, err := helpers.Seal(stagePurpose, t)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "livekitInit/stageEntranceURL could not seal the ticket of %s, so it records the grid: %v", room, err)
		return ""
	}
	if err := redisStore.SetStringWithTTL(ctx, registry.RecordingStageTicket, []string{room}, t.Nonce, life); err != nil {
		helpers.LogErrorWithContext(ctx, "livekitInit/stageEntranceURL could not keep the ticket of %s, so it records the grid: %v", room, err)
		return ""
	}
	return api + RecordingStagePath + ticket
}

// forgetStageTicket drops the ticket a room keeps, for a recording that
// draws the grid: no recorder may enter its stage.
func forgetStageTicket(ctx context.Context, room string) {
	_ = redisStore.Delete(ctx, registry.RecordingStageTicket, []string{room})
}

// ClaimStageTicket opens a ticket and claims it, once, and answers its room:
// sealed here, unexpired, and still its room's latest recording's ticket,
// taken in the same step. false for anything else, saying nothing of why.
func ClaimStageTicket(ctx context.Context, ticket string) (string, bool) {
	var t stageTicket
	if helpers.Unseal(stagePurpose, ticket, &t) != nil || t.Room == "" || t.Nonce == "" || time.Now().Unix() >= t.Exp {
		return "", false
	}
	claimed, err := redisStore.DeleteIfHeld(ctx, registry.RecordingStageTicket, []string{t.Room}, t.Nonce)
	return t.Room, err == nil && claimed
}

// RevokeStageTicket revokes the ticket of a recording that ended, while it is
// still its room's: read back from the request LiveKit's egress_ended webhook
// carries. A grid recording has none. Never logged.
func RevokeStageTicket(ctx context.Context, info *livekit.EgressInfo) {
	if !IsStageRecording(info) {
		return
	}
	u, err := url.Parse(info.GetRoomComposite().GetCustomBaseUrl())
	if err != nil {
		return
	}
	_, ticket, ok := strings.Cut(u.Path, RecordingStagePath)
	var t stageTicket
	if !ok || helpers.Unseal(stagePurpose, ticket, &t) != nil || t.Room == "" {
		return
	}
	_, _ = redisStore.DeleteIfHeld(ctx, registry.RecordingStageTicket, []string{t.Room}, t.Nonce)
}

// recorderGrant is what the recorder may do in a room: join it hidden, as a
// recorder, and subscribe; never publish, send data or change itself. LiveKit's
// own recorder's grant (protocol's egress.BuildEgressToken). Pure.
func recorderGrant(room string) *lka.VideoGrant {
	grant := &lka.VideoGrant{RoomJoin: true, Room: room, Hidden: true, Recorder: true}
	grant.SetCanSubscribe(true)
	grant.SetCanPublish(false)
	grant.SetCanPublishData(false)
	grant.SetCanUpdateOwnMetadata(false)
	return grant
}

// RecorderToken mints the recorder's token for a room (recorderGrant), as an
// egress with an identity of its own, for RecorderTokenTTL. LiveKit lists it
// hidden and as an egress, so it is no one in the call (IsPerson).
func (s *LiveKitServiceStruct) RecorderToken(room string) (string, error) {
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	return s.accessToken(recorderGrant(room), "recorder-"+hex.EncodeToString(id), "", RecorderTokenTTL).
		SetKind(livekit.ParticipantInfo_EGRESS).
		ToJWT()
}

// RecorderServerURL is the LiveKit server the recorder dials: the one this
// server talks to (LIVEKIT_HOST), as a websocket. On a stock install that is
// ws://livekit:7880, the address egress itself dials (its ws_url) from inside
// the Docker network the recorder's Chrome runs in. The layout page is https
// and this is ws: Chrome lets it only because both composes run egress with
// insecure: true (final-compose.yml, distribute-compose.yml). An install whose
// LIVEKIT_HOST egress can't reach records the grid (business/LiveKit's
// RecordAgainOnGrid). "" with no LiveKit.
func (s *LiveKitServiceStruct) RecorderServerURL() string {
	if s.Config == nil {
		return ""
	}
	return signalling.ToWebsocketURL(strings.TrimSpace(s.Config.HostURL))
}
