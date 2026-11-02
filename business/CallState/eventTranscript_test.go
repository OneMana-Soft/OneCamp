package business

import (
	"context"
	"errors"
	"testing"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// An event's call's transcript is read a page at a time from the recording
// filed under the events its "Recordings & notes" list (eventsOfCall: the
// event, its series, the first event that held its call), as its recording
// is found to play (meet-02, head C2). The page asked for reaches the read. A
// transcript of any other event's call, a deleted one, or none at all reads
// as none; a graph that doesn't answer is an error, never "none".
func TestAnEventsTranscriptIsReadOnlyUnderItsEvent(t *testing.T) {
	ctx := context.Background()
	ev, series, first, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	wasRead, wasMeeting := eventTranscriptPage, meetingOf
	t.Cleanup(func() { eventTranscriptPage, meetingOf = wasRead, wasMeeting })
	meetingOf = func(_ context.Context, id string) (*model.Meeting, error) {
		return &model.Meeting{ID: id, Event: first}, nil
	}
	filed := map[string]uuid.UUID{"EG_ev": ev, "EG_series": series, "EG_first": first, "EG_other": other}
	var asked []helpers.Page
	eventTranscriptPage = func(_ context.Context, key string, page helpers.Page) (*dgraphStruct.DgraphRecording, error) {
		asked = append(asked, page)
		if key == "EG_down" {
			return nil, errors.New("the graph didn't answer")
		}
		e, ok := filed[key]
		if !ok {
			return nil, recordingDomain.ErrNotFound
		}
		return &dgraphStruct.DgraphRecording{EgressId: key, Event: e.String(), TranscriptCount: 7}, nil
	}
	keys := calendarBusiness.CallKeys{Events: []uuid.UUID{ev, series}, Room: "meet-m1"}
	page := helpers.Page{Index: 2, Size: 3}
	for _, key := range []string{"EG_ev", "EG_series", "EG_first"} {
		asked = nil
		rec, err := EventTranscript(ctx, keys, key, page)
		if err != nil || rec == nil || rec.EgressId != key || rec.TranscriptCount != 7 {
			t.Errorf("EventTranscript(%s) = %+v, %v; want the event's transcript", key, rec, err)
		}
		if len(asked) != 1 || asked[0] != page {
			t.Errorf("EventTranscript(%s) read the pages %+v, want only %+v", key, asked, page)
		}
	}
	for _, key := range []string{"EG_other", "EG_gone"} {
		if rec, err := EventTranscript(ctx, keys, key, page); err != nil || rec != nil {
			t.Errorf("EventTranscript(%s) = %+v, %v; want none", key, rec, err)
		}
	}
	if _, err := EventTranscript(ctx, keys, "EG_down", page); err == nil {
		t.Error("EventTranscript while the graph doesn't answer = nil error, want its error")
	}
}
