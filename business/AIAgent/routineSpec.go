package business

// routineSpec.go — the pure, DB-free core for agent routines: validate and
// normalize a user's routine request (name, prompt, cadence, fire time) into a
// storable, always-sane shape before it touches Postgres or the scheduler. Kept
// pure so every edge (blank prompt, bogus cadence, out-of-range minute, missing
// name, timezone math) is exhaustively unit-testable without a database.

import (
	"fmt"
	"strings"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
)

const (
	routineMaxName   = 80
	routineMaxPrompt = 2000
	minutesPerDay    = 1440
)

// RoutineInput is a normalized routine request. AtMinuteUTC is minutes past UTC
// midnight (the fire time); Recurrence is an RRULE-lite rule the scheduler can
// fire (FREQ=DAILY or FREQ=WEEKLY[;BYDAY=...]).
type RoutineInput struct {
	Name        string
	Prompt      string
	Recurrence  string
	AtMinuteUTC int
}

// NormalizeRoutineInput validates and clamps a routine request into an
// always-storable shape, or returns an actionable error the agent can relay to
// the user. Total + pure: same input always yields the same output.
//
// Rules:
//   - Prompt is required (the standing instruction); trimmed and capped.
//   - Recurrence must be a cadence the scheduler can actually fire; normalized
//     to upper-case. An unfireable rule is rejected with guidance rather than
//     silently never running.
//   - AtMinuteUTC wraps into [0,1439] so a bad offset can't push the fire time
//     off the clock.
//   - Name defaults to a short slug of the prompt when omitted, and is capped.
func NormalizeRoutineInput(in RoutineInput) (RoutineInput, error) {
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return RoutineInput{}, fmt.Errorf("a routine needs a prompt (what should I do each time?)")
	}
	if len(prompt) > routineMaxPrompt {
		prompt = strings.TrimSpace(prompt[:routineMaxPrompt])
	}

	rule := strings.ToUpper(strings.TrimSpace(in.Recurrence))
	if rule == "" {
		return RoutineInput{}, fmt.Errorf("a routine needs a cadence (e.g. FREQ=DAILY, FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR, or FREQ=HOURLY;INTERVAL=2)")
	}
	if !schedulerBusiness.ValidRecurrence(rule) {
		return RoutineInput{}, fmt.Errorf("unsupported cadence %q (use FREQ=DAILY, FREQ=WEEKLY with an optional BYDAY list, or FREQ=HOURLY;INTERVAL=N)", in.Recurrence)
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = routineNameFromPrompt(prompt)
	}
	if len(name) > routineMaxName {
		name = strings.TrimSpace(name[:routineMaxName])
	}

	return RoutineInput{
		Name:        name,
		Prompt:      prompt,
		Recurrence:  rule,
		AtMinuteUTC: wrapMinute(in.AtMinuteUTC),
	}, nil
}

// wrapMinute folds any integer into a valid minute-of-day [0,1439], so a
// timezone conversion that crosses midnight (or a fat-fingered value) still
// lands on the clock rather than being rejected.
func wrapMinute(m int) int {
	m %= minutesPerDay
	if m < 0 {
		m += minutesPerDay
	}
	return m
}

// MinuteUTCFromLocal converts a local wall-clock time (hour, minute) at a given
// UTC offset (in minutes; e.g. -420 for US Pacific standard time) into minutes
// past UTC midnight, wrapping across the day boundary. Pure — lets the routine
// tool honor "9am Pacific" without the scheduler ever needing a timezone.
func MinuteUTCFromLocal(hour, minute, tzOffsetMinutes int) int {
	return wrapMinute(hour*60 + minute - tzOffsetMinutes)
}

// routineNameFromPrompt derives a short, human default name from the prompt
// (first several words), so a routine list is scannable even when the user
// didn't name it.
func routineNameFromPrompt(prompt string) string {
	words := strings.Fields(prompt)
	if len(words) > 6 {
		words = words[:6]
	}
	name := strings.Join(words, " ")
	if len(name) > routineMaxName {
		name = strings.TrimSpace(name[:routineMaxName])
	}
	return name
}
