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

// switchLocks serialises flips of one room's transcription switch, so the
// server's copy and the clients' (the room's metadata) are written in the same
// order: two people flipping it at once could otherwise leave one saying on and
// the other off.
var switchLocks helpers.KeyedLock

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
// member who is in it, and tells everyone in the call through the room's
// metadata.
//
// Anyone connected to the call may turn it either way: it is the call's
// switch, not the host's, and the notice names whoever did it. Guests have no
// session and can't reach this. Refused while the workspace's mode is off.
func SetTranscription(ctx context.Context, roomName, identity, byName string, on bool) (*TranscriptionSwitch, error) {
	roomName = strings.TrimSpace(roomName)
	if roomName == "" || strings.TrimSpace(identity) == "" || !livekitInit.LiveKitService.Ready() {
		return nil, ErrNotInCall
	}
	if transcriptionBusiness.Mode() == transcriptionBusiness.ModeOff {
		return nil, ErrTranscriptionDisabled
	}

	room, err := livekitInit.LiveKitService.GetRoom(ctx, roomName)
	if errors.Is(err, livekitInit.ErrRoomNotFound) {
		return nil, ErrNotInCall
	}
	if err != nil {
		return nil, err
	}
	present, err := livekitInit.LiveKitService.RoomHasParticipant(ctx, roomName, identity)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, ErrNotInCall
	}

	switchLocks.Acquire(roomName, 0)
	defer switchLocks.Release(roomName)

	sw := &TranscriptionSwitch{
		On:    on,
		By:    byName,
		ByID:  identity,
		Since: time.Now().UnixMilli(),
		Where: transcriptionBusiness.Where(),
		Kept:  keepsTranscript(roomName),
	}
	keep := func() error {
		return editCallState(ctx, roomName, room.Sid, func(st *callState) { st.Transcription = sw })
	}
	tell := func() error {
		_, err := livekitInit.LiveKitService.UpdateRoomMetadata(ctx, roomName, map[string]any{metaTranscription: sw})
		return err
	}
	// The order is the rule: everyone is told before a word is kept, and no
	// word is kept after it is turned off. So on: the call first, then the
	// server's copy; off: the server's copy first.
	steps := []func() error{tell, keep}
	if !on {
		steps = []func() error{keep, tell}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return sw, nil
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

// ForgetCall drops everything kept about the call that just ended in a room:
// its switches, and who was put out of it.
func ForgetCall(ctx context.Context, roomName string) {
	for _, spec := range []registry.Spec{registry.CallState, registry.CallRemoved} {
		if err := redisStore.Delete(ctx, spec, []string{roomName}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/ForgetCall could not clear %s for a room: %+v", spec.Namespace, err)
		}
	}
}
