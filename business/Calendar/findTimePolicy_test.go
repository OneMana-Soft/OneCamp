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

// onMemStore runs the package's own service, which Find a time and BusySeenBy
// read through, on a memory store for one test.
func onMemStore(t *testing.T) *memStore {
	t.Helper()
	m, prev := newMemStore(), stored
	stored = newService(m)
	t.Cleanup(func() { stored = prev })
	return m
}

// A workspace that keeps when members are busy to themselves (the
// calendar.freebusy.members policy off) keeps it from Find a time too: the
// asker's own busy times count, and nobody else's is read, named or used to
// place a slot, since a free slot is the rest of someone's day.
func TestFindTimeKeepsOthersBusyTimesToThemWhenTheWorkspaceDoes(t *testing.T) {
	onMemStore(t)
	me, maya := uuid.NewString(), uuid.NewString()
	start, end := at(9, 0), at(10, 0)
	prevEvents, prevGoogle, prevPolicy := eventsOf, googleBusyOf, freeBusyPolicy
	t.Cleanup(func() { eventsOf, googleBusyOf, freeBusyPolicy = prevEvents, prevGoogle, prevPolicy })
	eventsOf = func(_ context.Context, person string, _, _ *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
		if person == maya {
			return []*dgraphStruct.DgraphEvent{{Uuid: "dentist", StartTime: &start, EndTime: &end}}, nil
		}
		return nil, nil
	}
	googleBusyOf = func(context.Context, uuid.UUID, time.Time, time.Time) ([]integrationBusiness.BusySpan, error) {
		return nil, nil
	}
	now := time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC)
	hours := WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", TZ: "UTC"}
	ask := func() *FindTimeResult {
		t.Helper()
		res, err := FindTime(context.Background(), me, FindTimeInput{Participants: []string{maya}, DurationMinutes: 60, From: now, To: now.Add(12 * time.Hour), Hours: hours}, now)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	freeBusyPolicy = func(context.Context) bool { return true }
	if res := ask(); len(res.Busy[maya]) != 1 || res.Slots[0].Start.Equal(start) {
		t.Fatalf("with busy times shared, Maya's 9:00 should count: busy %v, first slot %v", res.Busy[maya], res.Slots[0])
	}

	freeBusyPolicy = func(context.Context) bool { return false }
	res := ask()
	if _, named := res.Busy[maya]; named {
		t.Errorf("with busy times kept private, Maya's are answered: %v", res.Busy[maya])
	}
	if len(res.Slots) == 0 || !res.Slots[0].Start.Equal(start) {
		t.Errorf("with busy times kept private, the first slot is %v; Maya's 9:00 shaped it", res.Slots)
	}
	if !reflect.DeepEqual(res.Private, []string{maya}) {
		t.Errorf("private %v, want Maya, whose times weren't counted", res.Private)
	}
}

// Everything that asks when people are busy (Find a time, the availability
// engine) reads through one rule, the calendar views' own: with the policy
// off, or for someone who is no member now, the asker's own times and nobody
// else's. The others are named as private, once each, and their calendars
// are never read.
func TestBusySeenByReadsOnlyTheAskerWhenTheWorkspaceKeepsBusyTimesPrivate(t *testing.T) {
	m := onMemStore(t)
	me, maya := uuid.NewString(), uuid.NewString()
	start, end := at(9, 0), at(10, 0)
	prevEvents, prevGoogle, prevPolicy := eventsOf, googleBusyOf, freeBusyPolicy
	t.Cleanup(func() { eventsOf, googleBusyOf, freeBusyPolicy = prevEvents, prevGoogle, prevPolicy })
	var read []string
	eventsOf = func(_ context.Context, person string, _, _ *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
		read = append(read, person)
		return []*dgraphStruct.DgraphEvent{{Uuid: "event-" + person, StartTime: &start, EndTime: &end}}, nil
	}
	googleBusyOf = func(context.Context, uuid.UUID, time.Time, time.Time) ([]integrationBusiness.BusySpan, error) {
		return nil, nil
	}
	ask := func() (map[string]Busy, []string) {
		t.Helper()
		read = nil
		busy, private, err := BusySeenBy(context.Background(), me, []string{me, maya, maya}, at(8, 0), at(18, 0))
		if err != nil {
			t.Fatal(err)
		}
		return busy, private
	}

	freeBusyPolicy = func(context.Context) bool { return true }
	busy, private := ask()
	if !reflect.DeepEqual(read, []string{me, maya}) || len(busy[maya].Intervals) != 1 || len(private) != 0 {
		t.Fatalf("with busy times shared, read %v, Maya's %v, private %v: want both read once and nobody private", read, busy[maya].Intervals, private)
	}

	freeBusyPolicy = func(context.Context) bool { return false }
	busy, private = ask()
	if !reflect.DeepEqual(read, []string{me}) {
		t.Errorf("with busy times kept private, read %v: only the asker's calendar may be", read)
	}
	if _, answered := busy[maya]; answered || len(busy[me].Intervals) != 1 {
		t.Errorf("with busy times kept private, answered %v: want the asker's own and nobody else's", busy)
	}
	if !reflect.DeepEqual(private, []string{maya}) {
		t.Errorf("private %v, want Maya once", private)
	}

	freeBusyPolicy = func(context.Context) bool { return true }
	m.notMembers[uuid.MustParse(me)] = true
	_, private = ask()
	if !reflect.DeepEqual(read, []string{me}) || !reflect.DeepEqual(private, []string{maya}) {
		t.Errorf("an asker who is no member now read %v, private %v: want only their own times", read, private)
	}
}
