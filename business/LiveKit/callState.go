package business

// What a call has switched on, and who may switch it.
//
// Transcription used to run in every call, for everyone on Chrome or Edge, from
// the moment their microphone was on: the browser's recogniser streamed their
// speech to Google's or Microsoft's speech service whether or not anyone had
// asked for captions, and nobody in the call was told. Now it is a switch that
// belongs to the call. Somebody in the call turns it on, deliberately; the
// server writes it into the room's metadata, which every client (guests too)
// receives and shows as a notice naming who turned it on and where the audio
// goes; and nothing transcribes, in a browser or on the server, while it is
// off. The server keeps its own copy, because it has to answer "is this call
// being transcribed?" on every transcript line without asking LiveKit, and
// refuses the line when the answer is no.

import (
	"context"
	"errors"
	"strings"
	"time"

	transcriptionBusiness "github.com/akashc777/OneCamp/business/Transcription"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// The answers a call gives to "may this happen here?". Controllers map each to
// its own status and code (see the CALLS-FE ↔ CALLS-BE contract in
// design/calls-vs-zoom/P0.md).
var (
	// ErrNotInCall: the person is not connected to that call (or there is no
	// call in that room).
	ErrNotInCall = errors.New("not in this call")
	// ErrTranscriptionDisabled: the workspace's transcription mode is off.
	ErrTranscriptionDisabled = errors.New("transcription is turned off for this workspace")
	// ErrTranscriptionOff: nobody has turned transcription on in this call.
	ErrTranscriptionOff = errors.New("transcription is off in this call")
	// ErrTranscriptNotKept: the call is transcribed for captions only, because
	// it has no conversation to keep a transcript in (an instant meeting).
	ErrTranscriptNotKept = errors.New("this call keeps no transcript")
	// ErrNotBrowserMode: a browser posted words while the workspace transcribes
	// on the server, or the server's transcriber posted while it is the
	// browsers' job. Either would file every line twice.
	ErrNotBrowserMode = errors.New("not this transcription mode")
)

// metaTranscription is the room metadata key the switch lives under.
const metaTranscription = "transcription"

// TranscriptionSwitch is a call's transcription switch, as every client sees it
// in the room's metadata.
type TranscriptionSwitch struct {
	On bool `json:"on"`
	// By and ByID are whoever last flipped it: who turned it on, or off.
	By    string `json:"by,omitempty"`
	ByID  string `json:"byId,omitempty"`
	Since int64  `json:"since,omitempty"` // unix ms of that flip
	// Where says where the audio goes while it is on
	// (transcriptionBusiness.Where), so the notice can say it.
	Where string `json:"where,omitempty"`
	// Kept says whether final lines are stored as the call's transcript.
	Kept bool `json:"kept"`
}

// callState is the server's copy of a call's switches, for the room session
// (LiveKit's room sid) it belongs to.
type callState struct {
	SessionID     string               `json:"sid"`
	Transcription *TranscriptionSwitch `json:"transcription,omitempty"`
	// RecordingEgress is the recording running in this session, which is where
	// transcript lines go while it runs.
	RecordingEgress string `json:"egress,omitempty"`
}

// callStateLocks serialises read-modify-writes of one room's state here.
var callStateLocks helpers.KeyedLock

// switchLocks serialises flips of one call's transcription switch, keyed by
// the call's own room, so the server's copy and the clients' (each room's
// metadata) are written in the same order: two people flipping it at once could
// otherwise leave one saying on and the other off, and a breakout room made
// meanwhile (FollowCall) could take the switch from before the flip.
var switchLocks helpers.KeyedLock

// callRooms answers every LiveKit room of the call going on in a room: the
// call's own room first, then its open breakout rooms (calls package C). Call
// State, which this package can't import, says how (OnCallRooms); until it
// does, a room is a call of its own.
var callRooms = func(_ context.Context, room string) ([]string, error) { return []string{room}, nil }

// OnCallRooms tells this package how to find all the rooms of a room's call.
func OnCallRooms(fn func(ctx context.Context, room string) ([]string, error)) {
	callRooms = fn
}

// callOwnRoom answers the own room of the call going on in a room, from the
// room's name alone: a breakout room's call's, any other room itself. Call
// State says how (OnCallOwnRoom), as for callRooms; until it does, a room is a
// call of its own. Pure.
var callOwnRoom = func(room string) string { return room }

// OnCallOwnRoom tells this package how to find the own room of a room's call.
func OnCallOwnRoom(fn func(room string) string) {
	callOwnRoom = fn
}

// keepsTranscript reports whether a call in this room has somewhere to keep a
// transcript: a channel, a DM or a group. An instant meeting has none until
// meetings get a home of their own, so it is transcribed for captions only.
// Pure.
func keepsTranscript(roomName string) bool {
	channelUUID, chatGroupID := helpers.ClassifyRoom(roomName)
	return channelUUID != "" || chatGroupID != ""
}

func loadCallState(ctx context.Context, roomName string) (callState, error) {
	var st callState
	if _, err := redisStore.GetJSON(ctx, registry.CallState, []string{roomName}, &st); err != nil {
		return callState{}, err
	}
	return st, nil
}

// editCallState changes a room's state for one session. A state left by an
// earlier session in the same room (its room_finished never arrived) is
// dropped rather than carried into this call.
func editCallState(ctx context.Context, roomName, sid string, edit func(*callState)) error {
	callStateLocks.Acquire(roomName, 0)
	defer callStateLocks.Release(roomName)
	st, err := loadCallState(ctx, roomName)
	if err != nil {
		return err
	}
	if st.SessionID != sid {
		st = callState{SessionID: sid}
	}
	edit(&st)
	return redisStore.SetJSON(ctx, registry.CallState, []string{roomName}, st)
}

// CallSwitches answers what the current session in a room has switched on: its
// transcription switch (off when nobody turned it on) and the recording that
// runs in it. A state from another session counts as nothing.
func CallSwitches(ctx context.Context, roomName, sid string) (TranscriptionSwitch, string) {
	st, err := loadCallState(ctx, roomName)
	if err != nil || sid == "" || st.SessionID != sid {
		return TranscriptionSwitch{}, ""
	}
	sw := TranscriptionSwitch{}
	if st.Transcription != nil {
		sw = *st.Transcription
	}
	return sw, st.RecordingEgress
}

// SetTranscription turns a call's transcription on or off, on behalf of a
// member who is in it, and tells everyone in the call through its rooms'
// metadata.
//
// Anyone connected to the call may turn it either way: it is the call's
// switch, not the host's, and the notice names whoever did it. Guests have no
// session and can't reach this. Refused while the workspace's mode is off.
//
// A call split into breakout rooms has one switch: a flip in any of its rooms
// is a flip in every one, so no room transcribes while the call's switch is
// off. A breakout room captions only and keeps no transcript, as it isn't
// recorded.
func SetTranscription(ctx context.Context, roomName, identity, byName string, on bool) (*TranscriptionSwitch, error) {
	roomName = strings.TrimSpace(roomName)
	if roomName == "" || strings.TrimSpace(identity) == "" || !livekitInit.LiveKitService.Ready() {
		return nil, ErrNotInCall
	}
	if transcriptionBusiness.Mode() == transcriptionBusiness.ModeOff {
		return nil, ErrTranscriptionDisabled
	}

	if _, err := livekitInit.LiveKitService.GetRoom(ctx, roomName); errors.Is(err, livekitInit.ErrRoomNotFound) {
		return nil, ErrNotInCall
	} else if err != nil {
		return nil, err
	}
	present, err := livekitInit.LiveKitService.RoomHasParticipant(ctx, roomName, identity)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, ErrNotInCall
	}

	// Turned off, it reaches at least the room it was asked in when the call's
	// other rooms can't be read; the caller is told, and asks again.
	rooms, failed := callRooms(ctx, roomName)
	if failed != nil {
		if on {
			return nil, failed
		}
		rooms = []string{roomName}
	}
	switchLocks.Acquire(rooms[0], 0)
	defer switchLocks.Release(rooms[0])

	sw := &TranscriptionSwitch{
		On:    on,
		By:    byName,
		ByID:  identity,
		Since: time.Now().UnixMilli(),
		Where: transcriptionBusiness.Where(),
		Kept:  keepsTranscript(rooms[0]),
	}
	// Every room is flipped, even past one that fails, so a failure never
	// leaves more of the call transcribing than it must; the caller retries.
	// The answer is the switch as the caller's own room has it.
	mine := *sw
	for i, room := range rooms {
		inRoom := *sw
		inRoom.Kept = i == 0 && sw.Kept
		if room == roomName {
			mine = inRoom
		}
		if err := switchRoom(ctx, room, &inRoom); err != nil && failed == nil {
			failed = err
		}
	}
	if failed != nil {
		return nil, failed
	}
	callChanged(ctx, rooms[0])
	return &mine, nil
}

// FollowCall gives a room its call's transcription switch: a breakout room as
// it is made, or a call's own room LiveKit made again while everyone was in
// its breakout rooms. Every room of a call has the switch the call was last
// given, so it is read off the first other room that has one. A room starts
// off, so nothing is written while the call's is off.
func FollowCall(ctx context.Context, roomName string) error {
	rooms, err := callRooms(ctx, roomName)
	if err != nil {
		return err
	}
	switchLocks.Acquire(rooms[0], 0)
	defer switchLocks.Release(rooms[0])
	for _, room := range rooms {
		if room == roomName {
			continue
		}
		lkRoom, err := livekitInit.LiveKitService.GetRoom(ctx, room)
		if errors.Is(err, livekitInit.ErrRoomNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		sw, _ := CallSwitches(ctx, room, lkRoom.Sid)
		if sw.Since == 0 {
			continue
		}
		if !sw.On {
			return nil
		}
		sw.Kept = roomName == rooms[0] && keepsTranscript(roomName)
		return switchRoom(ctx, roomName, &sw)
	}
	return nil
}

// switchRoom writes a call's switch into one of its rooms: the room's
// metadata, which everyone in it goes by, and the server's copy for the room's
// session. The order is the rule: everyone is told before a word is kept, and
// no word is kept after it is turned off. So on: the room first, then the
// server's copy; off: the server's copy first. A room LiveKit has closed (a
// call's own room while everyone is in its breakout rooms) has nobody to tell;
// it takes the switch again when it is made (FollowCall).
func switchRoom(ctx context.Context, roomName string, sw *TranscriptionSwitch) error {
	room, err := livekitInit.LiveKitService.GetRoom(ctx, roomName)
	if errors.Is(err, livekitInit.ErrRoomNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := func() error {
		return editCallState(ctx, roomName, room.Sid, func(st *callState) { st.Transcription = sw })
	}
	tell := func() error {
		_, err := livekitInit.LiveKitService.UpdateRoomMetadata(ctx, roomName, map[string]any{metaTranscription: sw})
		return err
	}
	steps := []func() error{tell, keep}
	if !sw.On {
		steps = []func() error{keep, tell}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// changeListeners are told when something a call says about itself changes.
var changeListeners []func(ctx context.Context, room string)

// OnCallChange adds a listener told whenever something about a room's call
// changes that the call says about itself: a recording started or stopped,
// transcription turned on or off. Call State listens, which this package
// can't import (business/CallState imports it). Added from init functions.
func OnCallChange(fn func(ctx context.Context, room string)) {
	changeListeners = append(changeListeners, fn)
}

func callChanged(ctx context.Context, room string) {
	for _, fn := range changeListeners {
		fn(ctx, room)
	}
}

// NoteRecording records the recording now running in a room's call, so its
// transcript lines are filed under it. Best-effort: a miss files them under
// the call session instead.
func NoteRecording(ctx context.Context, roomName, egressID string) {
	room, err := livekitInit.LiveKitService.GetRoom(ctx, roomName)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/NoteRecording could not read room %s: %+v", roomName, err)
		return
	}
	if err := editCallState(ctx, roomName, room.Sid, func(st *callState) { st.RecordingEgress = egressID }); err != nil {
		helpers.LogErrorWithContext(ctx, "business/NoteRecording could not note the recording: %+v", err)
	}
}

// ForgetRecording clears the room's running recording, when it is this one
// (or any, for an empty egress id).
func ForgetRecording(ctx context.Context, roomName, egressID string) {
	callStateLocks.Acquire(roomName, 0)
	defer callStateLocks.Release(roomName)
	st, err := loadCallState(ctx, roomName)
	if err != nil || st.RecordingEgress == "" || (egressID != "" && st.RecordingEgress != egressID) {
		return
	}
	st.RecordingEgress = ""
	if err := redisStore.SetJSON(ctx, registry.CallState, []string{roomName}, st); err != nil {
		helpers.LogErrorWithContext(ctx, "business/ForgetRecording could not clear the recording: %+v", err)
	}
}

// ForgetCall drops the switches kept for the call that just ended in a room.
// Who was put out of it is kept on the call's own record, which ended with it.
func ForgetCall(ctx context.Context, roomName string) {
	if err := redisStore.Delete(ctx, registry.CallState, []string{roomName}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/ForgetCall could not clear %s for a room: %+v", registry.CallState.Namespace, err)
	}
}
