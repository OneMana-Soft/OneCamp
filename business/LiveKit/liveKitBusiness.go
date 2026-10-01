package business

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
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

func GenerateToken(ctx context.Context, roomId string, userUUID string, userName string, isAdmin bool, audioEnabled bool, videoEnabled bool) (token string, err error) {
	token, err = livekitInit.LiveKitService.GenerateToken(roomId, userUUID, userName, isAdmin, audioEnabled, videoEnabled)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetMqttConfig Failed generate token err: %+v",
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

func CreateRoomAndGetToken(ctx context.Context, roomId string, userDgraphInfo *dgraphStruct.DgraphUser, isAdmin bool, audioEnabled bool, videoEnabled bool) (token string, alreadyExisted bool, err error) {

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

	token, err = GenerateToken(ctx, roomId, userDgraphInfo.Uid, userDgraphInfo.UserName, isAdmin, audioEnabled, videoEnabled)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateRoomAndGetToken Failed to generate token err: %+v",
			err,
		)
		return
	}

	return

}

func StartRecording(ctx context.Context, roomName string, userDgraphInfo *dgraphStruct.DgraphUser) (egresssInfo *livekit.EgressInfo, filePath string, err error) {
	egresssInfo, filePath, err = livekitInit.LiveKitService.StartRecording(ctx, roomName, userDgraphInfo.UserName)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/StartRecording Failed to start recording err: %+v",
			err,
		)
		return
	}

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

	return
}
