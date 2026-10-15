package business

// Putting someone out of a call.
//
// Nothing could. No screen offered it and no endpoint did it; revoking a guest
// link only marked the link revoked, so its guests stayed in the meeting and
// their token kept connecting for four hours; and a member removed from a
// channel stayed in its call with a token good for a day. Removal now goes
// through LiveKit's RemoveParticipant, whoever asks for it: a host, a revoked
// link, a channel removal.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// ErrRemovedFromCall: a host put this person out of the call that is still
// going on in the room.
var ErrRemovedFromCall = errors.New("removed from this call")

// Why someone was put out of a call, as the removed person's client is told.
const (
	RemovedByHost       = "host"
	RemovedLinkRevoked  = "link_revoked"
	RemovedNotAMember   = "not_member"
	callControlTopic    = "oc.call"
	callControlVersion  = 1
	callControlRemoved  = "removed"
	guestIdentityPrefix = "guest-"
)

// removedPacket is what the removed person's client receives, from the server
// (no sending participant), just before they are disconnected. Pure.
func removedPacket(byName, reason string) []byte {
	b, _ := json.Marshal(struct {
		V      int    `json:"v"`
		T      string `json:"t"`
		By     string `json:"by,omitempty"`
		Reason string `json:"reason"`
	}{callControlVersion, callControlRemoved, byName, reason})
	return b
}

// IsRemovedFromCall reports whether a host put this identity out of the call
// now going on in the room. A Redis that doesn't answer lets them back in: the
// alternative locks people out of every call whenever Redis blinks, and a host
// can remove them again.
func IsRemovedFromCall(ctx context.Context, roomName, identity string) bool {
	members, err := redisStore.GetSetMembers(ctx, registry.CallRemoved, []string{roomName})
	if err != nil {
		return false
	}
	return slices.Contains(members, identity)
}

// RemoveFromCall tells someone why, then puts them out of the call in a room.
// A host's removal also keeps them out of this call until it ends (their next
// token is refused, and a token they still hold is thrown out when it
// connects). Someone not connected is ErrNotInCall.
func RemoveFromCall(ctx context.Context, roomName, identity, byName, reason string) error {
	roomName = strings.TrimSpace(roomName)
	identity = strings.TrimSpace(identity)
	if roomName == "" || identity == "" || IsAgentIdentity(identity) || !livekitInit.LiveKitService.Ready() {
		return ErrNotInCall
	}
	present, err := livekitInit.LiveKitService.RoomHasParticipant(ctx, roomName, identity)
	if err != nil {
		return err
	}
	if !present {
		// Nobody to put out, and nothing to remember: someone who already left
		// is not barred from coming back.
		return ErrNotInCall
	}
	if reason == RemovedByHost {
		// Before the removal, so a reconnect racing it is refused too.
		if err := redisStore.AddToSet(ctx, registry.CallRemoved, []string{roomName}, identity); err != nil {
			helpers.LogErrorWithContext(ctx, "business/RemoveFromCall could not remember the removal: %+v", err)
		}
	}
	// Told first, so their screen can say why rather than "disconnected".
	// Best-effort: the removal matters more than the explanation.
	if err := livekitInit.LiveKitService.SendData(ctx, roomName, callControlTopic, removedPacket(byName, reason), identity); err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveFromCall could not tell %s why: %+v", identity, err)
	}
	return livekitInit.LiveKitService.RemoveParticipant(ctx, roomName, identity)
}

// RemoveGuestsWhere puts out of a room every connected guest for whom
// gone(identity) is true, with the reason given. Answers how many went.
func RemoveGuestsWhere(ctx context.Context, roomName, reason string, gone func(identity string) bool) int {
	if !livekitInit.LiveKitService.Ready() {
		return 0
	}
	people, err := livekitInit.LiveKitService.ConnectedParticipants(ctx, roomName)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveGuestsWhere could not list %s: %+v", roomName, err)
		return 0
	}
	n := 0
	for _, p := range people {
		if !strings.HasPrefix(p.Identity, guestIdentityPrefix) || !gone(p.Identity) {
			continue
		}
		if err := RemoveFromCall(ctx, roomName, p.Identity, "", reason); err != nil && !errors.Is(err, ErrNotInCall) {
			helpers.LogErrorWithContext(ctx, "business/RemoveGuestsWhere could not remove a guest from %s: %+v", roomName, err)
			continue
		}
		n++
	}
	return n
}

// CallRoomsWithPrefix lists the rooms that are live now whose names start with
// prefix (every instant meeting, for "meet-").
func CallRoomsWithPrefix(ctx context.Context, prefix string) ([]string, error) {
	if !livekitInit.LiveKitService.Ready() {
		return nil, nil
	}
	rooms, err := livekitInit.LiveKitService.ListAllRooms(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rooms {
		if r != nil && strings.HasPrefix(r.Name, prefix) {
			out = append(out, r.Name)
		}
	}
	return out, nil
}

// RemovedFromCallMsg is what someone a host put out of a call is told when
// they try to join it again.
const RemovedFromCallMsg = "You were removed from this call."
