package business

import (
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/CheckIn"
	"github.com/google/uuid"
)

// Asks is a check-in as a calendar shows it (calendar head 9, gcal-1b): its
// question, its channel, and each time it asks in a stretch of time.
type Asks struct {
	ID       uuid.UUID
	Channel  uuid.UUID
	Question string
	At       []time.Time
}

// running is the store's read, a variable a test can stand in for.
var running = model.Running

// AsksBetween is when the running check-ins of these channels ask, from
// `from` until before `to`: the times the sweeper asks them (Next), at most
// max for each. A paused or deleted check-in asks nothing, so it isn't
// listed, nor is one that asks no time in the stretch. One read.
func AsksBetween(channels []uuid.UUID, from, to time.Time, max int) ([]Asks, error) {
	out := []Asks{}
	if len(channels) == 0 || !to.After(from) || max <= 0 {
		return out, nil
	}
	list, err := running(channels)
	if err != nil {
		return nil, err
	}
	for i := range list {
		s, err := scheduleOf(&list[i])
		if err != nil {
			continue // a zone no longer known: the sweeper can't ask it either
		}
		if at := asksIn(s, from, to, max); len(at) > 0 {
			out = append(out, Asks{ID: list[i].Id, Channel: list[i].ChannelUUID, Question: list[i].Question, At: at})
		}
	}
	return out, nil
}

// asksIn is each time a schedule asks from `from` until before `to`, at most
// max. Pure.
func asksIn(s Schedule, from, to time.Time, max int) []time.Time {
	var at []time.Time
	t := from.Add(-time.Nanosecond) // Next is strictly after: a check-in at `from` counts
	for len(at) < max {
		next, err := Next(s, t)
		if err != nil || !next.Before(to) {
			break
		}
		at = append(at, next)
		t = next
	}
	return at
}
