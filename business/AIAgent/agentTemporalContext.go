package business

// Temporal grounding for agent runs.
//
// Symptom this fixes: asked "what did we ship TODAY", the agent answered
// "commits since March 15, 2024" — a date near the model's training cutoff.
// An LLM has no clock; nothing in the run told it the current date, so it
// guessed. Any relative time the user uses ("today", "yesterday", "this week",
// "recently", "last month") is unresolvable without an anchor.
//
// Fix: inject the current date/time (UTC, with weekday) into every run's system
// prompt so the model resolves relative times against reality instead of its
// training data. UTC is used because agent runs are event-driven/server-side
// and carry no reliable per-user timezone (unlike an interactive AI-chat
// request, which forwards the browser's tz); labeling it UTC lets the model
// reason about offsets when a user names one.

import (
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
)

// buildTemporalContext renders the "current date/time" system-prompt snippet
// from now. Pure and DB-free so it is unit-testable and deterministic. now is
// normalised to UTC so the output is stable regardless of the server's local
// zone.
func buildTemporalContext(now time.Time) string {
	u := now.UTC()
	// e.g. "Thursday, 02 Jan 2026 15:04 UTC"
	stamp := u.Format("Monday, 02 Jan 2006 15:04 MST")
	return "\n\nThe current date and time is " + stamp + ". This is the authoritative \"now\": " +
		"resolve every relative time the user uses (\"today\", \"yesterday\", \"this week\", \"recently\", " +
		"\"last month\", etc.) against it, and NEVER rely on your training data for the current date. " +
		"Times are in UTC unless the user names a timezone; if they do, convert from UTC accordingly. " +
		aiBusiness.UpcomingDays(u, 14) + " Look dates up in that list rather than counting."
}
