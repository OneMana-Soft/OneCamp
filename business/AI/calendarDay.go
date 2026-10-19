package business

// The assistant's calendar. The calendar tools (calendar_list_events,
// calendar_create_event), the briefing's Your day, What needs me and the daily
// note, which lists What needs me, all read and write the person's calendar
// the way the calendar itself does (business/Calendar): OneCamp events they
// made or were invited to, and their Google Calendar when it is linked.
//
// They used the Google connector alone. Without Google, on the demo and on any
// install without it, the assistant said Google "isn't connected yet" and
// never mentioned a meeting; with it, OneCamp's own events, a teammate's
// invitation and focus time were left out, and an event the assistant made
// went to Google, where no teammate was invited and OneCamp never showed it.

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// calendarEventsOf and createCalendarEvent are the calendar's own list and
// create; seams for tests.
var (
	calendarEventsOf    = calendarBusiness.GetEventsList
	createCalendarEvent = calendarBusiness.CreateEvent
	calendarEventByID   = calendarBusiness.GetEvent
)

type readerZoneKey struct{}

// WithReaderZone carries the time zone (an IANA name) of the person an answer
// is for, when their app says it, so their "today" is the day their calendar
// shows. A name that isn't a zone is ignored.
func WithReaderZone(ctx context.Context, tz string) context.Context {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return ctx
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return ctx
	}
	return context.WithValue(ctx, readerZoneKey{}, tz)
}

// readerZone is the reader's zone: the one WithReaderZone carries, else the
// one the assistant was told. known is false when neither said, and the zone
// is then UTC.
func readerZone(ctx context.Context) (loc *time.Location, known bool) {
	carried, _ := ctx.Value(readerZoneKey{}).(string)
	return helpers.FirstLocation(carried, ai.GetLocalization(ctx)["timezone"])
}

// calendarDayMax bounds the events "today's calendar" lists.
const calendarDayMax = 6

// calendarDay is the person's calendar for the rest of their day, soonest
// first: events not over yet, from now to the end of their day when their zone
// is known, and to the same time tomorrow when it isn't. Never errors: a
// calendar that can't be read lists nothing.
func calendarDay(ctx context.Context, userInfo *userModels.UserInfo, now time.Time) []adapter.BriefingDayItem {
	loc, known := readerZone(ctx)
	end := now.Add(24 * time.Hour)
	if known {
		end = helpers.DayStart(now, loc).AddDate(0, 0, 1)
	}
	events, err := calendarEventsOf(ctx, userInfo, &now, &end)
	if err != nil {
		helpers.LogInfoWithContext(ctx, "business/AI/calendarDay: the calendar couldn't be read: %v", err)
		return nil
	}
	today := now.In(loc).Format("2006-01-02")
	var day []*dgraphStruct.DgraphEvent
	for _, e := range events {
		switch {
		case e == nil || e.StartTime == nil || e.EndTime == nil:
		case e.AllDay:
			// A Google all-day event is its dates: today's, whatever its
			// placeholder times say.
			if e.StartDate <= today && today < e.EndDate {
				day = append(day, e)
			}
		case e.EndTime.After(now) && e.StartTime.Before(end):
			day = append(day, e)
		}
	}
	sortEvents(day)
	out := make([]adapter.BriefingDayItem, 0, min(len(day), calendarDayMax))
	for _, e := range day {
		if len(out) == calendarDayMax {
			break
		}
		out = append(out, adapter.BriefingDayItem{
			Source:   "calendar",
			Kind:     "event",
			Title:    eventTitle(e),
			Subtitle: eventStarts(e, loc, known),
			URL:      "/app/calendar/event/" + url.PathEscape(e.Uuid),
		})
	}
	return out
}

// sortEvents orders events soonest first, all-day ones at the head of their day.
func sortEvents(events []*dgraphStruct.DgraphEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.AllDay != b.AllDay && eventDay(a) == eventDay(b) {
			return a.AllDay
		}
		return a.StartTime.Before(*b.StartTime)
	})
}

// eventDay is the UTC date an event sorts under.
func eventDay(e *dgraphStruct.DgraphEvent) string {
	if e.AllDay {
		return e.StartDate
	}
	return e.StartTime.UTC().Format("2006-01-02")
}

func eventTitle(e *dgraphStruct.DgraphEvent) string {
	if t := strings.TrimSpace(e.Title); t != "" {
		return t
	}
	return "Untitled event"
}

// eventStarts is when an event starts, on the reader's clock: "Tue 15:00",
// or "All day". Without their zone the time is UTC, and says so.
func eventStarts(e *dgraphStruct.DgraphEvent, loc *time.Location, known bool) string {
	if e.AllDay {
		return "All day"
	}
	s := e.StartTime.In(loc).Format("Mon " + helpers.ClockLayout)
	if !known {
		s += " UTC"
	}
	return s
}

// eventSpan is an event's day and times for the assistant to read: "Tue 14
// Oct, 15:00–15:30", "Wed 14 Oct, all day" or, over days, "Tue 14 Oct,
// 15:00 – Wed 15 Oct, 10:00". Pure.
func eventSpan(e *dgraphStruct.DgraphEvent, loc *time.Location) string {
	if e.AllDay {
		first, err1 := time.Parse("2006-01-02", e.StartDate)
		after, err2 := time.Parse("2006-01-02", e.EndDate)
		if err1 != nil || err2 != nil {
			return "all day"
		}
		last := after.AddDate(0, 0, -1)
		if !last.After(first) {
			return first.Format("Mon 2 Jan") + ", all day"
		}
		return first.Format("Mon 2 Jan") + " to " + last.Format("Mon 2 Jan") + ", all day"
	}
	s, e2 := e.StartTime.In(loc), e.EndTime.In(loc)
	if s.Format("2006-01-02") == e2.Format("2006-01-02") {
		return fmt.Sprintf("%s, %s–%s", s.Format("Mon 2 Jan"), s.Format("15:04"), e2.Format("15:04"))
	}
	return fmt.Sprintf("%s – %s", s.Format("Mon 2 Jan, 15:04"), e2.Format("Mon 2 Jan, 15:04"))
}

// calendarEventFor is a OneCamp event the person asking is on, as the
// calendar reads it (business/Calendar.GetEvent, whichever store is on):
// "event not found" alike for one that isn't there and one that isn't
// theirs. A Google event's "gcal-" id isn't one: these read OneCamp's.
func calendarEventFor(ctx context.Context, userInfo *userModels.UserInfo, eventUUID string) (*dgraphStruct.DgraphEvent, error) {
	if _, err := uuid.Parse(eventUUID); err != nil && !calendarBusiness.IsInstanceID(eventUUID) {
		return nil, fmt.Errorf("event not found")
	}
	event, err := calendarEventByID(ctx, userInfo, eventUUID)
	if err != nil || event == nil {
		return nil, fmt.Errorf("event not found")
	}
	return event, nil
}
