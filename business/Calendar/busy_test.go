package Calendar

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// fakeBusy stands in for a person's OneCamp events and Google busy times.
func fakeBusy(t *testing.T, events []*dgraphStruct.DgraphEvent, google []integrationBusiness.BusySpan, googleErr error) {
	t.Helper()
	prevEvents, prevGoogle := eventsOf, googleBusyOf
	t.Cleanup(func() { eventsOf, googleBusyOf = prevEvents, prevGoogle })
	eventsOf = func(context.Context, string, *time.Time, *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
		return events, nil
	}
	googleBusyOf = func(context.Context, uuid.UUID, time.Time, time.Time) ([]integrationBusiness.BusySpan, error) {
		return google, googleErr
	}
}

func at(h, m int) time.Time { return time.Date(2026, 10, 14, h, m, 0, 0, time.UTC) }

func ivs(pairs ...time.Time) []Interval {
	var out []Interval
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Interval{Start: pairs[i], End: pairs[i+1]})
	}
	return out
}

// CAL-BUG-11 and 12: busy is OneCamp's events and the Google calendars.
func TestBusyCountsOneCampAndGoogle(t *testing.T) {
	s, e := at(9, 0), at(10, 0)
	fakeBusy(t, []*dgraphStruct.DgraphEvent{{Uuid: "standup", StartTime: &s, EndTime: &e}},
		[]integrationBusiness.BusySpan{{Start: at(14, 0), End: at(15, 0)}}, nil)
	b, err := ReadBusy(context.Background(), uuid.NewString(), at(0, 0), at(23, 0), BusyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := ivs(at(9, 0), at(10, 0), at(14, 0), at(15, 0)); !reflect.DeepEqual(b.Intervals, want) || b.GoogleUnread {
		t.Fatalf("busy %v unread %v, want %v and read", b.Intervals, b.GoogleUnread, want)
	}
	// BusyFor, booking pages' and Find a time's, is the same engine.
	got, err := BusyFor(context.Background(), uuid.NewString(), at(0, 0), at(23, 0))
	if err != nil || !reflect.DeepEqual(got, b) {
		t.Fatalf("BusyFor %v (%v), want ReadBusy's %v", got, err, b.Intervals)
	}
}

// CAL-BUG-12: a Google calendar that can't be read is unknown, never free,
// and hides nothing of OneCamp's own events.
func TestAnUnreadGoogleCalendarIsUnknown(t *testing.T) {
	s, e := at(9, 0), at(10, 0)
	fakeBusy(t, []*dgraphStruct.DgraphEvent{{Uuid: "standup", StartTime: &s, EndTime: &e}}, nil, errors.New("token revoked"))
	b, err := ReadBusy(context.Background(), uuid.NewString(), at(0, 0), at(23, 0), BusyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.GoogleUnread || !reflect.DeepEqual(b.Intervals, ivs(at(9, 0), at(10, 0))) {
		t.Fatalf("busy %v unread %v, want OneCamp's standup and unread", b.Intervals, b.GoogleUnread)
	}
}

// Rescheduling an event: it is no clash with itself, in OneCamp or in its
// Google copy, which FreeBusy can't name.
func TestTheEventBeingMovedIsNoClashWithItself(t *testing.T) {
	s, e := at(10, 0), at(11, 0)
	gid := "g-123"
	moving := &dgraphStruct.DgraphEvent{Uuid: "review", StartTime: &s, EndTime: &e, GoogleCalendarEventId: &gid}
	fakeBusy(t, []*dgraphStruct.DgraphEvent{moving},
		[]integrationBusiness.BusySpan{{Start: at(9, 30), End: at(11, 30)}}, nil)
	b, err := ReadBusy(context.Background(), uuid.NewString(), at(0, 0), at(23, 0), BusyOptions{Except: moving})
	if err != nil {
		t.Fatal(err)
	}
	if want := ivs(at(9, 30), at(10, 0), at(11, 0), at(11, 30)); !reflect.DeepEqual(b.Intervals, want) {
		t.Fatalf("busy %v, want the Google span without the event's own hour: %v", b.Intervals, want)
	}
}

func TestCutBusy(t *testing.T) {
	in := ivs(at(8, 0), at(9, 0), at(9, 30), at(12, 0), at(13, 0), at(14, 0))
	got := CutBusy(in, Interval{Start: at(10, 0), End: at(13, 30)})
	want := ivs(at(8, 0), at(9, 0), at(9, 30), at(10, 0), at(13, 30), at(14, 0))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cut %v, want %v", got, want)
	}
	if got := CutBusy(in, Interval{Start: at(10, 0), End: at(10, 0)}); !reflect.DeepEqual(got, in) {
		t.Fatal("an empty cut changed something")
	}
}

// Find a time and booking pages used BusyFor, which dropped GoogleUnread, so
// an unreadable Google calendar counted as free there while the assistant
// called it unknown. Find a time now names whose calendar it couldn't read.
func TestFindTimeSaysWhoseGoogleCalendarItCouldntRead(t *testing.T) {
	fakeBusy(t, nil, nil, errors.New("429 rate limited"))
	me := uuid.NewString()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC) // a Tuesday
	hours := WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", TZ: "UTC"}
	res, err := FindTime(context.Background(), me, FindTimeInput{DurationMinutes: 30, From: now, To: now.Add(48 * time.Hour), Hours: hours}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Unread, []string{me}) {
		t.Fatalf("unread %v, want %v", res.Unread, []string{me})
	}
	if b, err := BusyFor(context.Background(), me, now, now.Add(time.Hour)); err != nil || !b.GoogleUnread {
		t.Fatalf("BusyFor unread %v (%v), want unread", b.GoogleUnread, err)
	}
}
