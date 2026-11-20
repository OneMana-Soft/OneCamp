package business

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
)

func createLiveKitRoom(ctx context.Context, roomId string) (err error) {
	_, err = livekitInit.LiveKitService.CreateRoom(ctx, roomId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/createLiveKitRoom Failed to create livekit room err: %+v",
			err,
		)
		return
	}

	return
}

// GenerateToken mints a member's call token, letting them publish sources
// (SourcesAtDoor). language is what the person speaks (their browser's), for
// the server's transcriber; "" when unknown.
func GenerateToken(ctx context.Context, roomId string, identity string, userName string, isAdmin bool, language string, sources []string) (token string, err error) {
	token, err = livekitInit.LiveKitService.GenerateToken(roomId, identity, userName, isAdmin, language, sources)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GenerateToken Failed generate token err: %+v",
			err,
		)
		return
	}

	return
}

func CheckRoomExists(ctx context.Context, roomId string) (exists bool, err error) {
	rooms, err := livekitInit.LiveKitService.ListRooms(ctx, roomId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CheckRoomExists Failed to list livekit rooms err: %+v",
			err,
		)
		return
	}

	exists = len(rooms) > 0
	return
}

// IsCallActive reports whether a call is going on in a conversation's room: a
// person (livekitInit.IsPerson) is in one of the call's rooms. This is the
// signal for the "call active" dot. Unlike CheckRoomExists it is NOT fooled by
// the transcription agent, a recorder or a hidden watcher lingering in a room
// nobody is in, nor by LiveKit's post-call EmptyTimeout window during which
// the empty room object still exists.
//
// Use IsCallActive for any "should we show the call dot" decision. Reserve
// CheckRoomExists for room-lifecycle logic (e.g. deciding whether to create
// a new room vs reuse an existing one).
func IsCallActive(ctx context.Context, roomId string) (bool, error) {
	active, err := livekitInit.LiveKitService.RoomHasHumanParticipants(ctx, roomId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/IsCallActive Failed to check human participants err: %+v",
			err,
		)
		return false, err
	}
	return active || inBreakoutRooms(ctx, roomId), nil
}

// inBreakoutRooms reports whether a person is in one of the breakout rooms of
// the call in a room. While a call's people are all in its breakout rooms, its
// own room holds nobody, or only its recorder, or LiveKit has closed it: the
// call goes on all the same. Call State says which rooms the call has
// (callRooms), asked only when its own room has nobody in it. LiveKit lists
// the breakout rooms at once, and the dot asks who is in them only until it
// finds a person (RoomsHaveHumanParticipants). A room that can't be read
// counts as nobody's, as the sidebar takes it.
func inBreakoutRooms(ctx context.Context, room string) bool {
	rooms, err := callRooms(ctx, room)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/IsCallActive could not read the rooms of the call in %s: %+v", room, err)
		return false
	}
	breakouts := slices.DeleteFunc(slices.Clone(rooms), func(r string) bool { return r == room })
	in, err := livekitInit.LiveKitService.RoomsHaveHumanParticipants(ctx, breakouts...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/IsCallActive could not list the breakout rooms of the call in %s: %+v", room, err)
		return false
	}
	return in
}

// HumanActiveRoomNames returns the own rooms of the calls going on now, for
// the sidebar to light many conversations' dots in a single pass: a room with
// a person in it, and the call's own room of a breakout room with a person in
// it (callOwnRoom), as IsCallActive answers for one call. A whole answer is
// kept for activeRoomsTTL (activeRooms), and each load is handed its own copy.
func HumanActiveRoomNames(ctx context.Context) (map[string]bool, error) {
	activeRooms.Lock()
	kept, ok := activeRooms.kept.Get(activeRoomsKey)
	began := activeRooms.changes
	activeRooms.Unlock()
	if ok {
		return maps.Clone(kept), nil
	}
	rooms, complete, err := livekitInit.LiveKitService.HumanActiveRoomNames(ctx)
	if err != nil {
		return nil, err
	}
	active := make(map[string]bool, len(rooms))
	for room := range rooms {
		active[callOwnRoom(room)] = true
	}
	if complete {
		activeRooms.Lock()
		if activeRooms.changes == began {
			activeRooms.kept.Set(activeRoomsKey, maps.Clone(active))
		}
		activeRooms.Unlock()
	}
	return active, nil
}

// activeRooms keeps the sidebar's answer of which calls are going on
// (HumanActiveRoomNames) for activeRoomsTTL (H3C, 19 Nov 2026). The sidebar
// asked LiveKit for every room, then who was in each busy one, on every load
// of everybody's. One key: the answer is the call server's rooms, no person's.
//
// Emptied by CallRoomsChanged: the webhook's room_finished, participant_joined
// and participant_left, once the room is found ours. Only a whole answer is
// kept: a failed listing, or one missing a room whose people couldn't be read,
// serves its own load only, as does a read that began before a change.
//
// Stale: a dot lit up to 3 s after its call ended, or dark up to 3 s after one
// began, only where the call's event missed this process (another instance
// took the webhook, or it was lost). It decides nothing about who may join,
// ring, record or read a call (IsCallActive and CheckRoomExists ask LiveKit
// every time): it only lights rows of the reader's own sidebar.
var activeRooms = struct {
	sync.Mutex
	kept *helpers.TTLCache[map[string]bool]
	// changes counts CallRoomsChanged, so a read under way when a call
	// changes isn't kept.
	changes uint64
}{kept: helpers.NewTTLCacheClocked[map[string]bool](activeRoomsTTL, activeRoomsNow)}

// activeRoomsHeldAt, when set, is the time activeRooms ages by instead of the
// clock's: only HoldClockForTest, in the integration build, sets it.
var activeRoomsHeldAt atomic.Pointer[time.Time]

func activeRoomsNow() time.Time {
	if t := activeRoomsHeldAt.Load(); t != nil {
		return *t
	}
	return time.Now()
}

const (
	activeRoomsTTL = 3 * time.Second
	activeRoomsKey = "rooms"
)

// CallRoomsChanged empties the sidebar's kept answer of which calls are going
// on (activeRooms): a call of this workspace began or ended, or someone joined
// or left it.
func CallRoomsChanged() {
	activeRooms.Lock()
	defer activeRooms.Unlock()
	activeRooms.changes++
	activeRooms.kept.Purge()
}

// DeleteRoom force-closes a LiveKit room (evicting any remaining
// participants, including the transcription agent). Best-effort.
func DeleteRoom(ctx context.Context, roomId string) {
	if err := livekitInit.LiveKitService.DeleteRoom(ctx, roomId); err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteRoom Failed to delete livekit room %s err: %+v",
			roomId, err,
		)
	}
}

// IsAgentIdentity reports whether a participant identity belongs to the
// non-human transcription agent, where only an identity is known (a
// transcript's speakers, a removal). Someone LiveKit lists is asked
// livekitInit.IsPerson.
func IsAgentIdentity(identity string) bool {
	return livekitInit.IsAgentIdentity(identity)
}

// CreateRoomAndGetToken makes the call room when there is none and mints the
// member's token for it. Refused with ErrRemovedFromCall for someone a host put
// out of the call that is still going on there, and with ErrCallLocked for
// someone new while it is locked (both helpers.CallRefusal). isAdmin is an
// instant meeting's own host.
//
// What the person chose in the green room (microphone, camera) is not asked:
// it is the client's state, and the token always lets them change their mind
// (see livekitInit.CallPublishSources); only what the call holds is left out.
func CreateRoomAndGetToken(ctx context.Context, roomId string, userDgraphInfo *dgraphStruct.DgraphUser, isAdmin bool, language string) (token string, alreadyExisted bool, err error) {

	if userDgraphInfo == nil || userDgraphInfo.Uid == "" {
		return "", false, ErrNotInCall
	}
	if err := MayEnter(ctx, roomId, userDgraphInfo, isAdmin); err != nil {
		return "", false, err
	}
	sources, err := SourcesAtDoor(ctx, roomId, userDgraphInfo, isAdmin)
	if err != nil {
		return "", false, err
	}

	alreadyExisted, err = CheckRoomExists(ctx, roomId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateRoomAndGetToken Failed to check if room exists err: %+v",
			err,
		)
		return
	}

	if !alreadyExisted {
		err = createLiveKitRoom(ctx, roomId)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/CreateRoomAndGetToken Failed to create livekit room err: %+v",
				err,
			)
			return
		}
	}

	token, err = GenerateToken(ctx, roomId, userDgraphInfo.Uid, callName(roomId, userDgraphInfo), isAdmin, language, sources)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateRoomAndGetToken Failed to generate token err: %+v",
			err,
		)
		return
	}

	return

}

// meetingRoomPrefix starts the name of an instant meeting's room, the rooms
// guests join from a link (business/Guest's MeetingRoomPrefix; that package
// imports this one).
const meetingRoomPrefix = "meet-"

// callName is the name a member is shown by in a call, on their tile and as
// whoever started its recording: the one name rule (DgraphUser.DisplayName).
// In an instant meeting, which people from outside the workspace join, it
// stops before the rule's last step, part of the member's address, and is
// "Someone" when that leaves nothing. Pure.
func callName(roomName string, user *dgraphStruct.DgraphUser) string {
	if !strings.HasPrefix(roomName, meetingRoomPrefix) {
		return user.DisplayName()
	}
	var forGuests dgraphStruct.DgraphUser
	if user != nil {
		forGuests = *user
	}
	forGuests.EmailID = ""
	if name := forGuests.DisplayName(); name != "" {
		return name
	}
	return "Someone"
}

func StopRecording(ctx context.Context, roomName string) (err error) {
	// A person's Stop is final: nothing records the call again by itself,
	// even when its recording already ended with its room.
	if kerr := callStateModel.KeepRecording(ctx, roomName, nil); kerr != nil {
		helpers.LogErrorWithContext(ctx, "business/StopRecording could not note that nobody keeps the call in %s recorded: %+v", roomName, kerr)
	}
	_, err = livekitInit.LiveKitService.StopRecording(ctx, roomName)

	if err != nil {
		// Nothing recording is the asker's view being behind (ErrNothingRecording),
		// answered as such on every route: not an error of the server's to log.
		if !errors.Is(err, ErrNothingRecording) {
			helpers.LogErrorWithContext(ctx,
				"business/StopRecording Failed to stop recording err: %+v",
				err,
			)
		}
		return
	}

	ForgetRecording(ctx, roomName, "")
	callChanged(ctx, roomName)
	return
}
