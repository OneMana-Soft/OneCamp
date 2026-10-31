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
	meta := parseRoomMeta(room.Metadata)
	view.Recording = RecordingView{On: meta.IsRecording}
	if meta.IsRecording {
		view.Recording.By = meta.RecordingStartedBy
	}
	if meta.Transcription != nil {
		view.Transcription = *meta.Transcription
	}

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
