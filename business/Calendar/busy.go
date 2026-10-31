package Calendar

// When someone is busy: one engine for Find a time, booking pages (BusyFor)
// and the assistant's scheduling (business/AI), so all of them give the same
// answer for the same people.
//
// Busy is every OneCamp event a person created or was invited to, plus what
// their Google calendars show busy when they have linked Google: the primary
// calendar and every other one they have selected there, read with FreeBusy.
// Only start and end leave this file: never a title.

import (
	"context"
	"time"

	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Seams for tests: a person's OneCamp events, and their Google busy times.
var (
	eventsOf     = calendarEventsOf
	googleBusyOf = integrationBusiness.FetchUserGoogleBusy
)

// calendarEventsOf is a person's OneCamp events between from and to: from
// the one store, the ones that show as busy, when its events are there; from
// the graph otherwise.
func calendarEventsOf(ctx context.Context, userUUID string, from, to *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
	if helpers.CalendarReadsFromStore() {
		return stored.busyEvents(ctx, userUUID, *from, *to)
	}
	return graphEventsOf(ctx, userUUID, from, to)
}

// Busy is when someone is busy, and whether all of it could be read.
type Busy struct {
	// Intervals are merged and sorted (MergeBusy).
	Intervals []Interval
	// GoogleUnread is a linked Google calendar that couldn't be read, wholly
	// or in part: the time it would block is unknown. A caller that must never
	// call unknown time free checks it.
	GoogleUnread bool
	// Spans are the same time unmerged, each with what it is (calendar A3):
	// what says why a time isn't free, and the status chat shows, never a title.
	Spans []BusySpan
}

// What a busy span is: never its title.
const (
	BusyMeeting = "meeting"
	BusyFocus   = "focus"
	BusyAway    = "away"
	BusyGoogle  = "google"
)

// BusySpan is a stretch someone is busy, and what kind of busy it is.
type BusySpan struct {
	Interval
	Kind string
}

// BusyOptions changes what ReadBusy counts.
type BusyOptions struct {
	// Except leaves one OneCamp event out: the one being moved, which is no
	// clash with itself. FreeBusy can't name an event, so when the event has a
	// Google copy its time comes out of the Google busy times too.
	Except *dgraphStruct.DgraphEvent
}

// ReadBusy is when a person is busy between from and to. A Google calendar
// that can't be read hides nothing of OneCamp's own events: they are still
// counted, and GoogleUnread says the rest is unknown.
func ReadBusy(ctx context.Context, userUUID string, from, to time.Time, opts BusyOptions) (Busy, error) {
	events, err := eventsOf(ctx, userUUID, &from, &to)
	if err != nil {
		return Busy{}, err
	}
	var busy []Interval
	var spans []BusySpan
	for _, e := range events {
		if e == nil || e.StartTime == nil || e.EndTime == nil {
			continue
		}
		if e.DeletedAt != nil && e.DeletedAt.Year() > 1970 {
			continue
		}
		if opts.Except != nil && e.Uuid == opts.Except.Uuid {
			continue
		}
		busy = append(busy, Interval{Start: *e.StartTime, End: *e.EndTime})
		kind := BusyMeeting
		if e.IsAway != nil && *e.IsAway {
			kind = BusyAway
		} else if e.IsFocus != nil && *e.IsFocus {
			kind = BusyFocus
		}
		spans = append(spans, BusySpan{Interval: Interval{Start: *e.StartTime, End: *e.EndTime}, Kind: kind})
	}
	out := Busy{}
	if id, err := uuid.Parse(userUUID); err == nil {
		gspans, gerr := googleBusyOf(ctx, id, from, to)
		if gerr != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar/ReadBusy google err: %+v", gerr)
			out.GoogleUnread = true
		}
		google := make([]Interval, 0, len(gspans))
		for _, s := range gspans {
			google = append(google, Interval{Start: s.Start, End: s.End})
		}
		if e := opts.Except; e != nil && e.GoogleCalendarEventId != nil && *e.GoogleCalendarEventId != "" && e.StartTime != nil && e.EndTime != nil {
			google = CutBusy(google, Interval{Start: *e.StartTime, End: *e.EndTime})
		}
		busy = append(busy, google...)
		for _, g := range google {
			spans = append(spans, BusySpan{Interval: g, Kind: BusyGoogle})
		}
	}
	out.Intervals = MergeBusy(busy)
	out.Spans = spans
	return out, nil
}

// CutBusy takes one stretch of time out of busy intervals, splitting any that
// it falls inside. Pure.
func CutBusy(in []Interval, cut Interval) []Interval {
	if !cut.End.After(cut.Start) {
		return in
	}
	out := make([]Interval, 0, len(in)+1)
	for _, b := range in {
		if !b.Start.Before(cut.End) || !b.End.After(cut.Start) {
			out = append(out, b) // no overlap
			continue
		}
		if b.Start.Before(cut.Start) {
			out = append(out, Interval{Start: b.Start, End: cut.Start})
		}
		if b.End.After(cut.End) {
			out = append(out, Interval{Start: cut.End, End: b.End})
		}
	}
	return out
}
