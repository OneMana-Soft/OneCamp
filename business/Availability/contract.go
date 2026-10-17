// Package business (Availability) is the calendar's one availability engine
// (design/calendar-vs-gcal/PROGRAM.md §4.2 #41 and CAL4; A3 builds it): who is
// free when, for Find a time, booking pages, the assistant, chat's statuses and
// rooms, with the reason a time isn't free in words.
//
// DAY 0. The engine is business/Calendar's (availability.go over busy.go, the
// one W0 fixed): Busy reads each person as BusyFor does, Suggest is FindTime
// with the working hours the request carries (nobody has hours of their own
// yet: A3 stores them), and Explain answers ErrUnavailable, since nothing says
// why a time isn't free today. Nothing calls Times yet, so nothing changes by
// its existing. A3 moves the engine here and grows it behind the same names.
package business

import (
	"context"
	"errors"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
)

// Engine answers who is free when. It never says what anyone is doing.
type Engine interface {
	// Busy is when each person is busy between from and to.
	Busy(ctx context.Context, people []string, from, to time.Time) (map[string]calendarBusiness.Busy, error)
	// Suggest is the first times the asker and everyone the request names
	// are free.
	Suggest(ctx context.Context, askerUUID string, in calendarBusiness.FindTimeInput, now time.Time) (*calendarBusiness.FindTimeResult, error)
	// Explain says in words why a slot isn't free for a person.
	Explain(ctx context.Context, personUUID string, slot calendarBusiness.Interval) (string, error)
}

// ErrUnavailable is something the engine can't answer yet.
var ErrUnavailable = errors.New("availability: that isn't available yet")

// Times is the engine everything asks.
var Times Engine = todaysEngine{}

// todaysEngine is business/Calendar's engine.
type todaysEngine struct{}

func (todaysEngine) Busy(ctx context.Context, people []string, from, to time.Time) (map[string]calendarBusiness.Busy, error) {
	out := make(map[string]calendarBusiness.Busy, len(people))
	for _, p := range people {
		if _, read := out[p]; read {
			continue
		}
		b, err := calendarBusiness.BusyFor(ctx, p, from, to)
		if err != nil {
			return nil, err
		}
		out[p] = b
	}
	return out, nil
}

func (todaysEngine) Suggest(ctx context.Context, askerUUID string, in calendarBusiness.FindTimeInput, now time.Time) (*calendarBusiness.FindTimeResult, error) {
	return calendarBusiness.FindTime(ctx, askerUUID, in, now)
}

func (todaysEngine) Explain(context.Context, string, calendarBusiness.Interval) (string, error) {
	return "", ErrUnavailable
}
