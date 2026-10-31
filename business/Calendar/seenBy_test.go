package Calendar

import (
	"context"
	"reflect"
	"testing"
	"time"

	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// What a calendar says of someone (in a meeting, in focus, away) is when
// they're busy, so it follows the one rule (PeopleSeenBy): another person's
// is read only while the asker sees when others are busy, and the asker's
// own always is.
func TestAStatusIsSeenByTheCalendarsOneRule(t *testing.T) {
	onMemStore(t)
	me, maya := uuid.NewString(), uuid.NewString()
	start, end := at(9, 0), at(10, 0)
	prevEvents, prevGoogle, prevPolicy := eventsOf, googleBusyOf, freeBusyPolicy
	t.Cleanup(func() { eventsOf, googleBusyOf, freeBusyPolicy = prevEvents, prevGoogle, prevPolicy })
	var read []string
	eventsOf = func(_ context.Context, person string, _, _ *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
		read = append(read, person)
		return []*dgraphStruct.DgraphEvent{{Uuid: "standup-" + person, StartTime: &start, EndTime: &end}}, nil
	}
	googleBusyOf = func(context.Context, uuid.UUID, time.Time, time.Time) ([]integrationBusiness.BusySpan, error) {
		return nil, nil
	}
	now := at(9, 30)

	freeBusyPolicy = func(context.Context) bool { return true }
	st, private, err := StatusSeenBy(context.Background(), me, maya, now)
	if err != nil || private || st.Kind != StatusBusy {
		t.Fatalf("with busy times shared: %+v private %v err %v, want Maya in a meeting", st, private, err)
	}

	freeBusyPolicy = func(context.Context) bool { return false }
	read = nil
	st, private, err = StatusSeenBy(context.Background(), me, maya, now)
	if err != nil || !private || st.Kind != "" || len(read) != 0 {
		t.Errorf("with busy times kept private: %+v private %v err %v, read %v; want nothing read or said", st, private, err, read)
	}
	st, private, err = StatusSeenBy(context.Background(), me, me, now)
	if err != nil || private || st.Kind != StatusBusy {
		t.Errorf("the asker's own status: %+v private %v err %v", st, private, err)
	}
}

// Someone's working hours tell when their day is, so they follow the same
// rule: kept private, only the asker's own are read and answered.
func TestWorkingHoursAreSeenByTheCalendarsOneRule(t *testing.T) {
	onMemStore(t)
	me, maya := uuid.New(), uuid.New()
	prevHours, prevPolicy := hoursOfPeople, freeBusyPolicy
	t.Cleanup(func() { hoursOfPeople, freeBusyPolicy = prevHours, prevPolicy })
	var asked []uuid.UUID
	hoursOfPeople = func(_ context.Context, people []uuid.UUID) (map[uuid.UUID]WorkingHours, error) {
		asked = append([]uuid.UUID(nil), people...)
		out := map[uuid.UUID]WorkingHours{}
		for _, p := range people {
			out[p] = WorkingHours{TZ: "UTC", Week: map[string][]Span{"1": {{Start: "09:00", End: "17:00"}}}}
		}
		return out, nil
	}

	freeBusyPolicy = func(context.Context) bool { return true }
	hours, private, err := HoursSeenBy(context.Background(), me.String(), []uuid.UUID{me, maya})
	if err != nil || len(hours) != 2 || len(private) != 0 {
		t.Fatalf("with busy times shared: %v private %v err %v, want both", hours, private, err)
	}

	freeBusyPolicy = func(context.Context) bool { return false }
	hours, private, err = HoursSeenBy(context.Background(), me.String(), []uuid.UUID{me, maya})
	if err != nil || !reflect.DeepEqual(asked, []uuid.UUID{me}) || len(hours) != 1 || !reflect.DeepEqual(private, []string{maya.String()}) {
		t.Errorf("with busy times kept private: asked %v, answered %v, private %v, err %v; want only the asker's own", asked, hours, private, err)
	}
}
