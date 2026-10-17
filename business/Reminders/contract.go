// Package business (Reminders) is the calendar's reminders
// (design/calendar-vs-gcal/PROGRAM.md §4.2 #43; A5 builds it): defaults per
// calendar, a person's own per event, fired by a claimed sweep through the
// Notification Router with their holds, with Join when the event has a call.
//
// DAY 0. The calendar reminds nobody today (how-calendar-works-backend.md §9):
// Defaults are none, and Set answers ErrUnavailable, so that a reminder is
// never accepted that nothing would fire. Nothing calls Reminders yet, so
// nothing changes by its existing. A5 replaces both behind the same names.
package business

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Rule is one reminder.
type Rule struct {
	// MinutesBefore the event starts; for an all-day event, before 00:00 of
	// its date in the person's zone.
	MinutesBefore int `json:"minutes_before"`
	// Method is how: "inapp", "push" or "email".
	Method string `json:"method"`
}

// Planner keeps who is reminded of what, and when.
type Planner interface {
	// Defaults are a calendar's reminders for its events.
	Defaults(ctx context.Context, calendarID string) ([]Rule, error)
	// Set is a person's reminders for an event, in place of what they had.
	Set(ctx context.Context, eventID uuid.UUID, personUUID string, rules []Rule) error
}

// ErrUnavailable is something reminders can't do yet.
var ErrUnavailable = errors.New("reminders: that isn't available yet")

// Reminders is the planner everything asks.
var Reminders Planner = noReminders{}

// noReminders is today: none.
type noReminders struct{}

func (noReminders) Defaults(context.Context, string) ([]Rule, error) { return nil, nil }

func (noReminders) Set(context.Context, uuid.UUID, string, []Rule) error { return ErrUnavailable }
