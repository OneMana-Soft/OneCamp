package business

// An event's call's home (meet-02): what it makes is filed under the event,
// readable by whoever may read the event (the calendar's own rule), and its
// recap goes to the ONE conversation of the event's people (main's ruling 4
// (a)). business/LiveKit, which this package imports, learns from here which
// event a room's call is (OnCallEvent), so it keeps that call's transcript.

import (
	"context"
	"errors"
	"slices"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

func init() {
	liveKitBusiness.OnCallEvent(eventOfRoom)
}

// eventOfRoom is the calendar event whose call is going on in a room (its
// uuid), "" for none: a conversation's call, an instant meeting, a name no
// call uses.
func eventOfRoom(ctx context.Context, room string) (string, error) {
	home, err := HomeOf(ctx, room)
	if errors.Is(err, ErrNotACallRoom) {
		return "", nil
	}
	if err != nil || home.Event == uuid.Nil {
		return "", err
	}
	return home.Event.String(), nil
}

// ConversationOf is the ONE conversation of a set of people (members' uuids):
// two people's DM, three or more people's group, by the chat's own grouping
// ids, so the same people always have the same conversation. One person alone
// has none. Pure.
func ConversationOf(members []uuid.UUID) (Ref, bool) {
	ids := make([]string, 0, len(members))
	for _, m := range members {
		if id := m.String(); m != uuid.Nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	switch {
	case len(ids) == 2:
		return Ref{Kind: conversationBusiness.KindDM, ID: helpers.GetGroupingId(ids[0], ids[1])}, true
	case len(ids) > 2:
		id, err := helpers.GenerateGroupID(ids)
		if err != nil {
			return Ref{}, false
		}
		return Ref{Kind: conversationBusiness.KindGroup, ID: id}, true
	}
	return Ref{}, false
}

// eventCallArtefacts reads what calls filed under events (a seam for tests).
var eventCallArtefacts = recordingDomain.EventCallArtefacts

// EventArtefacts lists what an event's call made, for someone the calendar has
// let read the event (business/Calendar CallArtefactKeys): everything filed
// under the event, its series, and the first event that held its call, since
// an instance changed on its own and a series split in two share the call.
func EventArtefacts(ctx context.Context, keys calendarBusiness.CallKeys) ([]recordingDomain.CallArtefact, error) {
	events, err := eventsOfCall(ctx, keys)
	if err != nil {
		return nil, err
	}
	return eventCallArtefacts(ctx, events)
}

// eventsOfCall is every event an event's call files what it makes under: the
// event, its series, and the first event that held its call (keys.Room).
func eventsOfCall(ctx context.Context, keys calendarBusiness.CallKeys) ([]uuid.UUID, error) {
	events := slices.Clone(keys.Events)
	if keys.Room != "" {
		home, err := HomeOf(ctx, keys.Room)
		if err != nil && !errors.Is(err, ErrNotACallRoom) {
			return nil, err
		}
		if home.Event != uuid.Nil && !slices.Contains(events, home.Event) {
			events = append(events, home.Event)
		}
	}
	return events, nil
}

// eventRecordingForPlay reads a recording filed under an event (a seam for
// tests).
var eventRecordingForPlay = recordingDomain.GetEventRecordingForPlay

// EventRecording is the recording with this key (its egress id) that an
// event's call made, for someone the calendar has let read the event (keys:
// business/Calendar CallArtefactKeys), to play it: nil when there is none, it
// was deleted, or it is filed under any other event.
func EventRecording(ctx context.Context, keys calendarBusiness.CallKeys, key string) (*dgraphStruct.DgraphRecording, error) {
	return eventRecordingIn(ctx, keys, key, eventRecordingForPlay)
}

// eventTranscriptPage reads a page of a transcript filed under an event (a
// seam for tests).
var eventTranscriptPage = recordingDomain.GetEventRecordingTranscript

// EventTranscript is a page of the transcript of the recording with this key
// (its egress id) that an event's call made, for someone the calendar has let
// read the event (keys: business/Calendar CallArtefactKeys), found as
// EventRecording finds the recording it plays: nil when there is none, it was
// deleted, or it is filed under any other event.
func EventTranscript(ctx context.Context, keys calendarBusiness.CallKeys, key string, page helpers.Page) (*dgraphStruct.DgraphRecording, error) {
	return eventRecordingIn(ctx, keys, key, func(ctx context.Context, key string) (*dgraphStruct.DgraphRecording, error) {
		return eventTranscriptPage(ctx, key, page)
	})
}

// readRecording reads one recording by its key (its egress id).
type readRecording func(ctx context.Context, key string) (*dgraphStruct.DgraphRecording, error)

// eventRecordingIn is the recording with this key, as read finds it, when it
// is filed under one of the events an event's call files under
// (eventsOfCall): nil when read finds none, or it is filed under any other
// event. A read that fails is an error, never "none".
func eventRecordingIn(ctx context.Context, keys calendarBusiness.CallKeys, key string, read readRecording) (*dgraphStruct.DgraphRecording, error) {
	events, err := eventsOfCall(ctx, keys)
	if err != nil {
		return nil, err
	}
	rec, err := read(ctx, key)
	if errors.Is(err, recordingDomain.ErrNotFound) {
		return nil, nil
	}
	if err != nil || rec == nil {
		return nil, err
	}
	if event, perr := uuid.Parse(rec.Event); perr != nil || !slices.Contains(events, event) {
		return nil, nil
	}
	return rec, nil
}

// The reads and writes DeleteEventArtefact and RestoreEventArtefact use
// (seams for tests): a deleted recording filed under an event, and the
// archive's own write of a recording's deletion, stamped or cleared.
var (
	deletedEventRecording = recordingDomain.GetDeletedEventRecording
	deleteRecordings      = recordingDomain.BulkArchiveRecordings
	restoreRecordings     = recordingDomain.BulkRestoreRecordings
)

// DeleteEventArtefact deletes what an event's call made under this key, its
// recording and transcript, for someone the calendar lets change the event
// (keys: business/Calendar CallArtefactKeysToChange). The delete is soft: it
// stamps the recording's deletion with the archive's own write, so the
// event's "Recordings & notes" and its player stop showing it at once,
// RestoreEventArtefact brings it back (the web's Undo), and the workspace's
// purge setting removes it for good, as it does any archived recording.
// false when no live recording with this key is filed under the events the
// call files under.
func DeleteEventArtefact(ctx context.Context, keys calendarBusiness.CallKeys, key string) (bool, error) {
	return changeEventArtefact(ctx, keys, key, eventRecordingForPlay, deleteRecordings)
}

// RestoreEventArtefact brings back what DeleteEventArtefact deleted, for the
// same people: false when no deleted recording with this key is filed under
// the events the call files under.
func RestoreEventArtefact(ctx context.Context, keys calendarBusiness.CallKeys, key string) (bool, error) {
	return changeEventArtefact(ctx, keys, key, deletedEventRecording, restoreRecordings)
}

// changeEventArtefact finds a recording of the event's call by read and
// writes it by write: false, and nothing written, when read finds none.
func changeEventArtefact(ctx context.Context, keys calendarBusiness.CallKeys, key string, read readRecording, write func(context.Context, []string) error) (bool, error) {
	rec, err := eventRecordingIn(ctx, keys, key, read)
	if err != nil || rec == nil {
		return false, err
	}
	if err := write(ctx, []string{rec.EgressId}); err != nil {
		return false, err
	}
	return true, nil
}
