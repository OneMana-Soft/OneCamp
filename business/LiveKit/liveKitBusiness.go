package business

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"

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

// GenerateToken mints a member's call token. language is what the person
// speaks (their browser's), for the server's transcriber; "" when unknown.
func GenerateToken(ctx context.Context, roomId string, identity string, userName string, isAdmin bool, language string) (token string, err error) {
	token, err = livekitInit.LiveKitService.GenerateToken(roomId, identity, userName, isAdmin, language)

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

// IsCallActive reports whether a room has a live human call in progress
// (≥1 non-agent participant connected). This is the correct signal for the
// FE "call active" indicator — unlike CheckRoomExists it is NOT fooled by
// the transcription agent lingering in an otherwise-empty room, nor by
// LiveKit's post-call EmptyTimeout window during which the empty room
// object still exists.
//
// Use IsCallActive for any "should we show the call dot" decision. Reserve
// CheckRoomExists for room-lifecycle logic (e.g. deciding whether to create
// a new room vs reuse an existing one).
func IsCallActive(ctx context.Context, roomId string) (active bool, err error) {
	active, err = livekitInit.LiveKitService.RoomHasHumanParticipants(ctx, roomId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/IsCallActive Failed to check human participants err: %+v",
			err,
		)
		return
	}
	return
}

// IsCallActiveExcluding is like IsCallActive but ignores the participant
// with identity excludeIdentity. Used by the participant_left webhook so a
// just-departed participant that LiveKit hasn't fully removed yet doesn't
// keep the call falsely "active".
func IsCallActiveExcluding(ctx context.Context, roomId string, excludeIdentity string) (active bool, err error) {
	active, err = livekitInit.LiveKitService.RoomHasHumanParticipantsExcluding(ctx, roomId, excludeIdentity)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/IsCallActiveExcluding Failed to check human participants err: %+v",
			err,
		)
		return
	}
	return
}

// HumanActiveRoomNames returns the set of room names that currently have a
// live human call. Used by the sidebar hydration path to compute
// call-active state for many conversations in a single pass.
func HumanActiveRoomNames(ctx context.Context) (map[string]bool, error) {
	return livekitInit.LiveKitService.HumanActiveRoomNames(ctx)
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
// non-human transcription agent. Used by webhook handlers to ignore the
// agent's own join/leave events when deciding if a human call is active.
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
// (see livekitInit.callPublishSources).
func CreateRoomAndGetToken(ctx context.Context, roomId string, userDgraphInfo *dgraphStruct.DgraphUser, isAdmin bool, language string) (token string, alreadyExisted bool, err error) {

	if userDgraphInfo == nil || userDgraphInfo.Uid == "" {
		return "", false, ErrNotInCall
	}
	if err := MayEnter(ctx, roomId, userDgraphInfo, isAdmin); err != nil {
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

	token, err = GenerateToken(ctx, roomId, userDgraphInfo.Uid, callName(roomId, userDgraphInfo), isAdmin, language)

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

func StartRecording(ctx context.Context, roomName string, userDgraphInfo *dgraphStruct.DgraphUser) (egresssInfo *livekit.EgressInfo, filePath string, err error) {
	egresssInfo, filePath, err = livekitInit.LiveKitService.StartRecording(ctx, roomName, callName(roomName, userDgraphInfo))
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/StartRecording Failed to start recording err: %+v",
			err,
		)
		return
	}

	// While it runs, the call's transcript lines are filed under it, and the
	// call's record lists it.
	NoteRecording(ctx, roomName, egresssInfo.EgressId)
	var by *uuid.UUID
	if userDgraphInfo != nil {
		if id, perr := uuid.Parse(userDgraphInfo.Uuid); perr == nil {
			by = &id
		}
	}
	if _, ferr := callStateModel.RecordingStarted(ctx, roomName, egresssInfo.EgressId, by, time.Now()); ferr != nil {
		helpers.LogErrorWithContext(ctx, "business/StartRecording could not file the recording under its call: %+v", ferr)
	}
	callChanged(ctx, roomName)
	return
}

func StopRecording(ctx context.Context, roomName string) (err error) {
	_, err = livekitInit.LiveKitService.StopRecording(ctx, roomName)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/StopRecording Failed to stop recording err: %+v",
			err,
		)
		return
	}

	ForgetRecording(ctx, roomName, "")
	callChanged(ctx, roomName)
	return
}
