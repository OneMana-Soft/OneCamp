// Package business (Availability) is the calendar's one availability engine
// (design/calendar-vs-gcal/PROGRAM.md §4.2 #41 and CAL4; A3 builds it): who is
// free when, for Find a time, booking pages, the assistant, chat's statuses and
// rooms, with the reason a time isn't free in words.
//
// Busy reads each person the asker may see (BusySeenBy: the workspace's
// busy-times policy, Find a time's rule), and Suggest is FindTime with the
// hours the request carries (the assistant's ask); Suggestions (engine.go)
// ranks times in each person's own hours and zone, with reasons, and Explain
// says why a slot isn't free for someone (calendar A3).
package business

import (
	"context"
	"errors"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/google/uuid"
)

// Engine answers who is free when. It never says what anyone is doing.
type Engine interface {
	// Busy is when each person is busy between from and to, as the asker may
	// see it (calendarBusiness.BusySeenBy). Private is whoever's times the
	// workspace keeps to themselves: never read.
	Busy(ctx context.Context, askerUUID string, people []string, from, to time.Time) (busy map[string]calendarBusiness.Busy, private []string, err error)
	// Suggest is the first times the asker and everyone the request names
	// are free.
	Suggest(ctx context.Context, askerUUID string, in calendarBusiness.FindTimeInput, now time.Time) (*calendarBusiness.FindTimeResult, error)
	// Explain says in words why a slot isn't free for a person, as the asker
	// may see it: ErrPrivate when the workspace keeps their times from them.
	Explain(ctx context.Context, askerUUID, personUUID string, slot calendarBusiness.Interval) (string, error)
}

// ErrPrivate is a person whose busy times the workspace keeps from the asker.
var ErrPrivate = errors.New("availability: their busy times are private")

// Times is the engine everything asks.
var Times Engine = todaysEngine{}

// todaysEngine is business/Calendar's engine.
type todaysEngine struct{}

func (todaysEngine) Busy(ctx context.Context, askerUUID string, people []string, from, to time.Time) (map[string]calendarBusiness.Busy, []string, error) {
	return calendarBusiness.BusySeenBy(ctx, askerUUID, people, from, to)
}

func (todaysEngine) Suggest(ctx context.Context, askerUUID string, in calendarBusiness.FindTimeInput, now time.Time) (*calendarBusiness.FindTimeResult, error) {
	return calendarBusiness.FindTime(ctx, askerUUID, in, now)
}

// Explain says why a slot isn't free for a person, in words, or that it is
// (calendar A3): away, in focus time or a meeting (never its title), or
// outside their working hours; free when none of these. Only for someone the
// asker may see when they're busy (calendarBusiness.PeopleSeenBy): anyone
// else is ErrPrivate, unread.
func (todaysEngine) Explain(ctx context.Context, askerUUID, personUUID string, slot calendarBusiness.Interval) (string, error) {
	id, err := uuid.Parse(personUUID)
	if err != nil {
		return "", refused("That person isn't in this workspace.")
	}
	seen, _, err := peopleSeenBy(ctx, askerUUID, []string{personUUID})
	if err != nil {
		return "", err
	}
	if len(seen) == 0 {
		return "", ErrPrivate
	}
	b, err := busyFor(ctx, personUUID, slot.Start, slot.End)
	if err != nil {
		return "", err
	}
	hours, err := hoursOf(ctx, []uuid.UUID{id})
	if err != nil {
		return "", err
	}
	var h *calendarBusiness.WorkingHours
	if kept, ok := hours[id]; ok {
		h = &kept
	}
	return explain(b, h, slot, b.GoogleUnread), nil
}

// explain is Explain's words, from what was read. Pure.
func explain(b calendarBusiness.Busy, h *calendarBusiness.WorkingHours, slot calendarBusiness.Interval, unread bool) string {
	rank := map[string]int{calendarBusiness.BusyAway: 4, calendarBusiness.BusyFocus: 3, calendarBusiness.BusyMeeting: 2, calendarBusiness.BusyGoogle: 2}
	kind := ""
	for _, s := range b.Spans {
		if s.Start.Before(slot.End) && s.End.After(slot.Start) && rank[s.Kind] > rank[kind] {
			kind = s.Kind
		}
	}
	switch kind {
	case calendarBusiness.BusyAway:
		return "Away then"
	case calendarBusiness.BusyFocus:
		return "In focus time then"
	case calendarBusiness.BusyMeeting, calendarBusiness.BusyGoogle:
		return "In a meeting then"
	}
	if i := 0; clashes(outsideHours(h, slot.Start, slot.End), &i, slot.Start, slot.End) {
		return "Outside their working hours"
	}
	if unread {
		return "Free in OneCamp; their Google Calendar couldn't be read"
	}
	return "Free then"
}
