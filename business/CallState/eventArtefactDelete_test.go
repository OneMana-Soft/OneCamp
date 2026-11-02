package business

import (
	"context"
	"errors"
	"testing"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// What an event's call made (its recording and transcript) is deleted and
// brought back only under the events its "Recordings & notes" list
// (eventsOfCall), as it is played. The delete is soft: it stamps the
// recording's deletion with the archive's own write, so the list and the
// player stop showing it at once, and Archive and the purge setting see it as
// any archived recording; the restore clears the stamp. A key filed under
// another event, a deleted one deleted again, a live one asked back, or none
// at all writes nothing and is not found; a graph that doesn't answer, or a
// write that fails, is an error, never "not found".
func TestAnEventsArtefactIsDeletedAndRestoredOnlyUnderItsEvent(t *testing.T) {
	ctx := context.Background()
	ev, first, other := uuid.New(), uuid.New(), uuid.New()
	wasLive, wasGone, wasDelete, wasRestore, wasMeeting := eventRecordingForPlay, deletedEventRecording, deleteRecordings, restoreRecordings, meetingOf
	t.Cleanup(func() {
		eventRecordingForPlay, deletedEventRecording, deleteRecordings, restoreRecordings, meetingOf = wasLive, wasGone, wasDelete, wasRestore, wasMeeting
	})
	meetingOf = func(_ context.Context, id string) (*model.Meeting, error) {
		return &model.Meeting{ID: id, Event: first}, nil
	}
	// filed is the event each key is filed under; gone holds those deleted.
	filed := map[string]uuid.UUID{"EG_ev": ev, "EG_first": first, "EG_other": other}
	gone := map[string]bool{}
	read := func(deleted bool) func(context.Context, string) (*dgraphStruct.DgraphRecording, error) {
		return func(_ context.Context, key string) (*dgraphStruct.DgraphRecording, error) {
			if key == "EG_down" {
				return nil, errors.New("the graph didn't answer")
			}
			e, ok := filed[key]
			if !ok || gone[key] != deleted {
				return nil, recordingDomain.ErrNotFound
			}
			return &dgraphStruct.DgraphRecording{EgressId: key, Event: e.String()}, nil
		}
	}
	eventRecordingForPlay, deletedEventRecording = read(false), read(true)
	var wrote []string
	write := func(deleted bool) func(context.Context, []string) error {
		return func(_ context.Context, keys []string) error {
			for _, k := range keys {
				gone[k] = deleted
				wrote = append(wrote, k)
			}
			return nil
		}
	}
	deleteRecordings, restoreRecordings = write(true), write(false)
	keys := calendarBusiness.CallKeys{Events: []uuid.UUID{ev}, Room: "meet-m1"}

	// The event's own, and that of the first event that held its call.
	for _, key := range []string{"EG_ev", "EG_first"} {
		if found, err := DeleteEventArtefact(ctx, keys, key); err != nil || !found || !gone[key] {
			t.Errorf("DeleteEventArtefact(%s) = %v, %v (deleted: %v); want it deleted", key, found, err, gone[key])
		}
		if found, err := DeleteEventArtefact(ctx, keys, key); err != nil || found {
			t.Errorf("DeleteEventArtefact(%s) again = %v, %v; want not found", key, found, err)
		}
		if found, err := RestoreEventArtefact(ctx, keys, key); err != nil || !found || gone[key] {
			t.Errorf("RestoreEventArtefact(%s) = %v, %v (deleted: %v); want it back", key, found, err, gone[key])
		}
		if found, err := RestoreEventArtefact(ctx, keys, key); err != nil || found {
			t.Errorf("RestoreEventArtefact(%s) of a live one = %v, %v; want not found", key, found, err)
		}
	}

	// Another event's, live or deleted, and a key that is nobody's.
	wrote = nil
	for _, key := range []string{"EG_other", "EG_none"} {
		if found, err := DeleteEventArtefact(ctx, keys, key); err != nil || found {
			t.Errorf("DeleteEventArtefact(%s) = %v, %v; want not found", key, found, err)
		}
	}
	gone["EG_other"] = true
	for _, key := range []string{"EG_other", "EG_none"} {
		if found, err := RestoreEventArtefact(ctx, keys, key); err != nil || found {
			t.Errorf("RestoreEventArtefact(%s) = %v, %v; want not found", key, found, err)
		}
	}
	if len(wrote) != 0 {
		t.Errorf("asks outside the event's call wrote %v, want nothing", wrote)
	}

	// A graph that doesn't answer, and a write that fails, are errors.
	if _, err := DeleteEventArtefact(ctx, keys, "EG_down"); err == nil {
		t.Error("DeleteEventArtefact while the graph doesn't answer = nil error, want its error")
	}
	if _, err := RestoreEventArtefact(ctx, keys, "EG_down"); err == nil {
		t.Error("RestoreEventArtefact while the graph doesn't answer = nil error, want its error")
	}
	deleteRecordings = func(context.Context, []string) error { return errors.New("the write failed") }
	if _, err := DeleteEventArtefact(ctx, keys, "EG_ev"); err == nil {
		t.Error("DeleteEventArtefact whose write failed = nil error, want its error")
	}
}
