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

// An event's call's recording is found by the events it is filed under (head
// D), the ones its "Recordings & notes" list (eventsOfCall): the event, its
// series, the first event that held its call. A recording of any other event,
// a deleted one, or none at all is not found; a graph that doesn't answer is
// an error, never "none".
func TestAnEventsRecordingIsFoundOnlyUnderItsEvent(t *testing.T) {
	ctx := context.Background()
	ev, series, first, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	wasRead, wasMeeting := eventRecordingForPlay, meetingOf
	t.Cleanup(func() { eventRecordingForPlay, meetingOf = wasRead, wasMeeting })
	meetingOf = func(_ context.Context, id string) (*model.Meeting, error) {
		return &model.Meeting{ID: id, Event: first}, nil
	}
	filed := map[string]uuid.UUID{"EG_ev": ev, "EG_series": series, "EG_first": first, "EG_other": other}
	eventRecordingForPlay = func(_ context.Context, key string) (*dgraphStruct.DgraphRecording, error) {
		if key == "EG_down" {
			return nil, errors.New("the graph didn't answer")
		}
		e, ok := filed[key]
		if !ok {
			return nil, recordingDomain.ErrNotFound
		}
		return &dgraphStruct.DgraphRecording{EgressId: key, Event: e.String()}, nil
	}
	keys := calendarBusiness.CallKeys{Events: []uuid.UUID{ev, series}, Room: "meet-m1"}
	for _, key := range []string{"EG_ev", "EG_series", "EG_first"} {
		if rec, err := EventRecording(ctx, keys, key); err != nil || rec == nil || rec.EgressId != key {
			t.Errorf("EventRecording(%s) = %+v, %v; want the event's recording", key, rec, err)
		}
	}
	for _, key := range []string{"EG_other", "EG_gone"} {
		if rec, err := EventRecording(ctx, keys, key); err != nil || rec != nil {
			t.Errorf("EventRecording(%s) = %+v, %v; want none", key, rec, err)
		}
	}
	if _, err := EventRecording(ctx, keys, "EG_down"); err == nil {
		t.Error("EventRecording while the graph doesn't answer = nil error, want its error")
	}
}
