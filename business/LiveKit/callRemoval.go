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
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// ErrRemovedFromCall: a host put this person out of the call that is still
// going on in the room. A refusal every join endpoint answers in its words
// (helpers.WriteCallRefusal).
var ErrRemovedFromCall = &helpers.CallRefusal{Code: "removed_from_call", Msg: RemovedFromCallMsg}

// Why someone was put out of a call, as the removed person's client is told.
const (
	RemovedByHost       = "host"
	RemovedLinkRevoked  = "link_revoked"
	RemovedNotAMember   = "not_member"
	guestIdentityPrefix = "guest-"
)

// CallControlTopic is the data topic on which the server tells one person, or
// everyone, what happened to them in a call: removed, muted, asked to unmute,
// given a part in it, the call ended. Only the server sends on it, and a
// client believes nothing on it from a participant (lib/calls/signals).
const CallControlTopic = "oc.call"

// CallPacket is one message on CallControlTopic. T says what happened; the
// rest is what it needs: who did it (a name), why, what was muted, the part
// someone was given.
type CallPacket struct {
	V      int    `json:"v"`
	T      string `json:"t"`
	By     string `json:"by,omitempty"`
	Reason string `json:"reason,omitempty"`
	Source string `json:"source,omitempty"`
	Role   string `json:"role,omitempty"`
}

// callPacketVersion is the packets' version: a client ignores a newer one it
// can't read rather than guess.
const callPacketVersion = 1

// packetBytes is a packet as it travels, with its version. Pure.
func packetBytes(p CallPacket) ([]byte, error) {
	p.V = callPacketVersion
	return json.Marshal(p)
}

// TellInCall sends a packet from the server to the given people in a room, or
// to everyone in it when there are none.
func TellInCall(ctx context.Context, roomName string, p CallPacket, identities ...string) error {
	b, err := packetBytes(p)
	if err != nil {
		return err
	}
	return livekitInit.LiveKitService.SendData(ctx, roomName, CallControlTopic, b, identities...)
}

// IsRemovedFromCall reports whether a host put this identity out of the call
// now going on in the room (Call State keeps it, for that call only), and an
// error when the record didn't answer, which the caller holds against them
// (fail closed): it used to let them back in.
func IsRemovedFromCall(ctx context.Context, roomName, identity string) (bool, error) {
	s, err := callStateModel.OpenSessionOf(ctx, roomName)
	if noRecord(err) {
		return false, nil
	}
	if err != nil || s == nil {
		return false, err
	}
	a, err := callStateModel.AttendeeOf(ctx, s.ID, identity)
	if err != nil {
		return false, err
	}
	return a != nil && a.RemovedBy != "", nil
}

// Remover is who put someone out of a call: their identity in it, and the
// name the removed person is told. Empty for the server's own removals.
type Remover struct {
	Identity string
	Name     string
}

// RemoveFromCall tells someone why, then puts them out of the call in a room.
// A host's removal also keeps them out of this call until it ends (their next
// token is refused, and a token they still hold is thrown out when it
// connects): kept out first, and never put out without it, as they could come
// straight back (ErrCallUnchecked, and the host tries again). Someone not
// connected is ErrNotInCall.
func RemoveFromCall(ctx context.Context, roomName, identity string, by Remover, reason string) error {
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
		if err := markRemoved(ctx, roomName, identity, by); err != nil {
			return err
		}
	}
	// Told first, so their screen can say why rather than "disconnected".
	// Best-effort: the removal matters more than the explanation.
	if err := TellInCall(ctx, roomName, CallPacket{T: "removed", By: by.Name, Reason: reason}, identity); err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveFromCall could not tell %s why: %+v", identity, err)
	}
	return livekitInit.LiveKitService.RemoveParticipant(ctx, roomName, identity)
}

// markRemoved keeps a removed person out of the call going on in a room until
// it ends, on their row in Call State, or answers ErrCallUnchecked: the record
// didn't answer, or holds no call to keep them out of yet (its first join not
// recorded), and they must not be put out only to come straight back.
func markRemoved(ctx context.Context, roomName, identity string, by Remover) error {
	s, err := callStateModel.OpenSessionOf(ctx, roomName)
	if noRecord(err) {
		// No Call State at all (a tool, a unit test): nothing can be kept.
		return nil
	}
	if err == nil && s == nil {
		err = errors.New("no call recorded in the room yet")
	}
	if err == nil {
		role := callStateModel.RoleParticipant
		if strings.HasPrefix(identity, guestIdentityPrefix) {
			role = callStateModel.RoleGuest
		}
		// Never empty: an empty mark keeps nobody out.
		who := by.Identity
		if who == "" {
			who = RemovedByHost
		}
		err = callStateModel.MarkRemoved(ctx, callStateModel.Attendee{SessionID: s.ID, Identity: identity, Role: role}, who, time.Now())
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveFromCall could not keep %s out of %s: %+v", identity, roomName, err)
		return ErrCallUnchecked
	}
	return nil
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
		if err := RemoveFromCall(ctx, roomName, p.Identity, Remover{}, reason); err != nil && !errors.Is(err, ErrNotInCall) {
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
