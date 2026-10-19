// Package business (Recurrence) is the calendar's recurrence engine
// (design/calendar-vs-gcal/PROGRAM.md §4.2 #39): RFC 5545 rules, expanded in
// their series' own zone, cut for "this and following", and described in
// words, for events and reminders, and with Options.Clamp for task repeats
// once they move onto it.
//
// Built here rather than taken from a library: what must be exactly right is
// the RFC's own rules (a time a clock change skips moves forward by the gap,
// one it repeats is its first, a month without the 31st has no 31st), and
// those are the ones a library chooses for itself.
//
// Task repeats and scheduled jobs still use business/Scheduler's RRULE-lite,
// whose month-end clamping is a deliberate task rule (the 31st repeats on 28
// February). Options.Clamp reproduces it (TestClampIsTodaysTaskRepeat), so the
// Scheduler can become a caller of this engine with no change of behaviour.
package business

import "time"

// Engine reads and expands repeat rules.
type Engine interface {
	// Parse reads an RRULE value; an *ErrRule says what can't be read.
	Parse(text string) (Rule, error)
	// Expand is the starts of the instances from from to to, earliest first.
	Expand(r Rule, start time.Time, loc *time.Location, from, to time.Time, o Options) []time.Time
	// Next is the first instance strictly after after.
	Next(r Rule, start time.Time, loc *time.Location, after time.Time, o Options) (time.Time, bool)
	// Describe is the rule in words ("Weekly on Tuesday").
	Describe(r Rule, start time.Time, loc *time.Location) string
}

// Rules is the engine everything asks.
var Rules Engine = rfc5545{}

// rfc5545 is this package's engine.
type rfc5545 struct{}

func (rfc5545) Parse(text string) (Rule, error) { return Parse(text) }

func (rfc5545) Expand(r Rule, start time.Time, loc *time.Location, from, to time.Time, o Options) []time.Time {
	return Expand(r, start, loc, from, to, o)
}

func (rfc5545) Next(r Rule, start time.Time, loc *time.Location, after time.Time, o Options) (time.Time, bool) {
	return Next(r, start, loc, after, o)
}

func (rfc5545) Describe(r Rule, start time.Time, loc *time.Location) string {
	return Describe(r, start, loc)
}
