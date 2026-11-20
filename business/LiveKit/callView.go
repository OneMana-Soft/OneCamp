package business

// What a call looks like from outside it: whether it is on, who is in it,
// whether it is being recorded or transcribed. The green room shows this
// before anyone joins, so nobody walks into a recorded or transcribed call
// without being told (CALL-BUG-12, CALL-BUG-2).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
)

// RecordingView is whether a call is being recorded, and by whom.
type RecordingView struct {
	On bool   `json:"on"`
	By string `json:"by,omitempty"`
}

// CallPerson is someone in a call, as the green room lists them.
type CallPerson struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CallView is a call as the green room sees it.
type CallView struct {
	Room          string              `json:"room"`
	Active        bool                `json:"active"`
	Recording     RecordingView       `json:"recording"`
	Transcription TranscriptionSwitch `json:"transcription"`
	Participants  []CallPerson        `json:"participants,omitempty"`
}

// roomMetaView is the part of a room's metadata the green room reads.
type roomMetaView struct {
	IsRecording        bool                 `json:"isRecording"`
	RecordingStartedBy string               `json:"recordingStartedBy"`
	EgressID           string               `json:"egressID"`
	Transcription      *TranscriptionSwitch `json:"transcription"`
}

// parseRoomMeta reads a room's metadata, treating anything unreadable as
// nothing switched on. Pure.
func parseRoomMeta(metadata string) roomMetaView {
	var m roomMetaView
	if strings.TrimSpace(metadata) != "" {
		_ = json.Unmarshal([]byte(metadata), &m)
	}
	return m
}

// OnNow is what a call's room has switched on now, as its metadata says: the
// recording (who started it, and its egress once there is one) and the
// captions switch. A consent record says what was on from here, read at the
// moment it is written, never from what the person's request claims.
type OnNow struct {
	Recording RecordingView
	// Egress is the running recording's egress id: "" while none runs, and
	// for the seconds before a new one's id lands.
	Egress        string
	Transcription TranscriptionSwitch
}

// onNowOf is what a room's metadata says is on; anything unreadable is
// nothing. Pure.
func onNowOf(metadata string) OnNow {
	meta := parseRoomMeta(metadata)
	var on OnNow
	if meta.IsRecording {
		on.Recording = RecordingView{On: true, By: meta.RecordingStartedBy}
		on.Egress = meta.EgressID
	}
	if meta.Transcription != nil {
		on.Transcription = *meta.Transcription
	}
	return on
}

// ReadOnNow is what a room has on now. A room with no call, or a server with
// no LiveKit, has nothing on.
func ReadOnNow(ctx context.Context, roomName string) (OnNow, error) {
	if !livekitInit.LiveKitService.Ready() {
		return OnNow{}, nil
	}
	room, err := livekitInit.LiveKitService.GetRoom(ctx, roomName)
	if errors.Is(err, livekitInit.ErrRoomNotFound) {
		return OnNow{}, nil
	}
	if err != nil {
		return OnNow{}, err
	}
	return onNowOf(room.Metadata), nil
}

// ReadCallView answers what is on in a room's call. withPeople lists who is in
// it (members' green room; a guest's page gets no names). A room with no call
// is an inactive view, not an error.
func ReadCallView(ctx context.Context, roomName string, withPeople bool) (CallView, error) {
	view := CallView{Room: roomName}
	if !livekitInit.LiveKitService.Ready() {
		return view, nil
	}
	room, err := livekitInit.LiveKitService.GetRoom(ctx, roomName)
	if errors.Is(err, livekitInit.ErrRoomNotFound) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	on := onNowOf(room.Metadata)
	view.Recording, view.Transcription = on.Recording, on.Transcription

	people, err := livekitInit.LiveKitService.ConnectedParticipants(ctx, roomName)
	if err != nil {
		return view, err
	}
	for _, p := range people {
		if !livekitInit.IsPerson(p) {
			continue
		}
		view.Active = true
		if withPeople {
			view.Participants = append(view.Participants, CallPerson{ID: p.Identity, Name: p.Name})
		}
	}
	return view, nil
}
