// Package business (Recurrence) is the calendar's recurrence engine
// (design/calendar-vs-gcal/PROGRAM.md §4.2 #39; A1 builds it): RFC 5545 rules,
// expanded in their series' own zone, split for "this and following", and
// described in words, for events, reminders and task repeats.
//
// DAY 0. The server's one rule engine is business/Scheduler's RRULE-lite,
// which task repeats and scheduled jobs use: Next is its NextRun. Nothing on
// the server describes a rule in words yet (the web app describes task repeats,
// lib/tasks/recurrence.ts), so Describe answers ErrUnavailable. Nothing calls
// Rules yet, so nothing changes by its existing. A1 replaces both behind the
// same names, and the Scheduler becomes a caller of it.
package business

import (
	"errors"
	"time"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
)

// Engine reads repeat rules.
type Engine interface {
	// Next is the first time rule fires strictly after after, for a series
	// whose time that just went by was prev. The zero time for a rule it
	// can't read, or that has ended.
	Next(rule string, after, prev time.Time) (time.Time, error)
	// Describe is rule in words ("Weekly on Tuesday").
	Describe(rule string) (string, error)
}

// ErrUnavailable is something the engine can't do yet.
var ErrUnavailable = errors.New("recurrence: that isn't available yet")

// Rules is the engine everything asks.
var Rules Engine = todaysRules{}

// todaysRules is the Scheduler's RRULE-lite.
type todaysRules struct{}

func (todaysRules) Next(rule string, after, prev time.Time) (time.Time, error) {
	return schedulerBusiness.NextRun(rule, after, prev)
}

func (todaysRules) Describe(string) (string, error) {
	return "", ErrUnavailable
}
