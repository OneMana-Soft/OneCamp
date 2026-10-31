package business

// A call's recording goes on after its breakout rooms (meet-10, zoom-02). A
// recording is LiveKit's egress in the call's own room, and LiveKit closes that
// room 20 seconds after its last person leaves it for a breakout room
// (livekitInit's roomDepartureGrace): a recorder isn't a person, so it doesn't
// hold the room open, and the recording ends with the room. Nothing recorded the
// call again when everyone came back, so its record had one recording that
// stopped at the rooms. Now the call records again, as whoever started its
// recording, once its own room is made again and the cut recording's end is
// known. A person's Stop clears who keeps the call recorded
// (model.KeepRecording), so a stopped recording never comes back.

import (
	"context"
	"strings"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
)

// resumeLocks keeps two starts of one call's recording from racing: its room
// made again (restoreRoom) and the cut recording's end (OnRecordingEnded) can
// arrive together.
var resumeLocks helpers.KeyedLock

// OnRecordingEnded stamps a recording's end, length, size and how it ended
// (egress_ended).
//
// A failed egress (out of memory on a small box, storage refused the upload)
// used to be stamped like a good one: it was listed at full length and played
// a black frame. Its status is kept now, and lists and the player say it
// failed. One that captured nothing (LiveKit aborts an egress stopped before
// anything came through: "Start signal not received") is empty, 0 s long, and
// says so: it isn't a fault. A webhook without its egress is ignored rather
// than dereferenced.
//
// A recording LiveKit cut short by closing the call's own room while everyone
// was in its breakout rooms may end after they are back (an upload takes a
// while): if the room has been made again since, the call records again there
// (resumeRecording).
func OnRecordingEnded(ctx context.Context, info *livekit.EgressInfo) {
	if info == nil || strings.TrimSpace(info.EgressId) == "" {
		return
	}
	status, durationSec, fileSize := recordingBusiness.EndOfEgress(info)
	// LiveKit's reason goes to the log, never on screen.
	switch {
	case recordingBusiness.Failed(status):
		helpers.LogErrorWithContext(ctx, "business/CallControl recording %s ended %s: %s", info.EgressId, info.Status, info.Error)
	case recordingBusiness.Empty(status):
		helpers.LogInfoWithContext(ctx, "business/CallControl recording %s captured nothing (%s: %s)", info.EgressId, info.Status, info.Error)
	}
	_ = recordingBusiness.UpdateStopRecordingTime(ctx, info.EgressId, durationSec, fileSize, info.StartedAt, status)
	// The call's record lists it with how it ended.
	if err := model.RecordingEnded(ctx, info.EgressId, status, time.Now()); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not note the end of recording %s on its call: %+v", info.EgressId, err)
	}
	// Its transcript lines go back to the call's own session from here.
	liveKitBusiness.ForgetRecording(ctx, info.RoomName, info.EgressId)

	room, recorded := info.RoomName, info.RoomId
	helpers.GoSafeNamed("calls.recording.again", func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		resumeWhereMadeAgain(ctx, room, recorded)
	})
}

// resumeWhereMadeAgain is resumeRecording for the call in a room whose
// recording of an earlier LiveKit room of that name (recorded, the room's sid)
// just ended: the call has taken on the room made again since, so everyone
// came back from its breakout rooms before this end reached us. A recording
// that ended in the room the call is in now is the call's own business: it
// was stopped, or it failed.
func resumeWhereMadeAgain(ctx context.Context, room, recorded string) {
	if room == "" || recorded == "" || callStateBusiness.IsBreakoutRoom(room) {
		return
	}
	open, err := model.OpenSessionOf(ctx, room)
	if err != nil || open == nil || open.RoomSID == "" || open.RoomSID == recorded {
		return
	}
	s, err := callStateBusiness.Get(ctx, open.ID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not read the call in %s to record it again: %+v", room, err)
		return
	}
	resumeRecording(ctx, s)
}

// resumeRecording records a call again in its own room, made again after
// LiveKit closed it while everyone was in the call's breakout rooms. It records
// as the person who keeps the call recorded, the way they started it, so the
// call's record and its channel both list both parts. It does nothing for a
// call nobody keeps recorded, while a recording of the call runs or its end
// hasn't reached us (OnRecordingEnded comes back here), or for a breakout
// room's call (rooms aren't recorded). Only a channel's call has both rooms
// and a recording: a DM's or a group's call has no rooms, and an instant
// meeting isn't recorded.
func resumeRecording(ctx context.Context, s *callStateBusiness.Session) {
	if s == nil || !s.Live || s.Parent != "" || s.Resource.Kind != conversationBusiness.KindChannel {
		return
	}
	id, err := uuid.Parse(s.ID)
	if err != nil {
		return
	}
	resumeLocks.Acquire(s.Room, 0)
	defer resumeLocks.Release(s.Room)
	by, running, err := model.RecordingWanted(ctx, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not read who keeps the call in %s recorded: %+v", s.Room, err)
		return
	}
	if by == nil || running {
		return
	}
	who, err := userBusiness.GetDgraphUserInfoByUUID(ctx, by.String())
	if err != nil || who == nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not find %s to record the call in %s again: %+v", by, s.Room, err)
		return
	}
	if err := channelBusiness.StartRecordingChannelCall(ctx, s.Room, who); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not record the call in %s again after its breakout rooms: %+v", s.Room, err)
	}
}
