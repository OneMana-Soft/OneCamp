package business

import (
	"strconv"
	"strings"
	"time"
)

// NextRun computes the next fire time for a recurring job given an RRULE-lite
// string. We support the subset that covers Slack's recurring reminders:
//
//	FREQ=DAILY
//	FREQ=WEEKLY;BYDAY=MO,WE,FR
//	FREQ=WEEKLY            (same weekday as the previous run)
//	FREQ=MONTHLY           (same day-of-month)
//	FREQ=HOURLY
//	INTERVAL=2             (every N periods; defaults to 1)
//
// `from` is "now" (when the job just fired) and `prev` is the run time that
// just elapsed; the next occurrence is always strictly after `from`. Returns
// the zero time if the rule is empty/invalid so the caller marks the job done.
func NextRun(rule string, from time.Time, prev time.Time) (time.Time, error) {
	parts := parseRule(rule)
	freq := parts["FREQ"]
	if freq == "" {
		return time.Time{}, nil
	}

	interval := 1
	if v, ok := parts["INTERVAL"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = n
		}
	}

	// Preserve the wall-clock time-of-day from the previous run so a daily
	// 9am reminder stays at 9am regardless of when the worker actually fired.
	base := prev
	if base.IsZero() {
		base = from
	}

	switch strings.ToUpper(freq) {
	case "HOURLY":
		next := base
		for !next.After(from) {
			next = next.Add(time.Duration(interval) * time.Hour)
		}
		return next, nil

	case "DAILY":
		next := base
		for !next.After(from) {
			next = next.AddDate(0, 0, interval)
		}
		return next, nil

	case "WEEKLY":
		return nextWeekly(parts["BYDAY"], base, from, interval), nil

	case "MONTHLY":
		next := base
		for !next.After(from) {
			next = next.AddDate(0, interval, 0)
		}
		return next, nil

	default:
		return time.Time{}, nil
	}
}

// nextWeekly handles FREQ=WEEKLY with optional BYDAY. When BYDAY lists multiple
// days, we pick the earliest day strictly after `from` (cycling to the next
// week if needed), preserving the time-of-day of `base`.
func nextWeekly(byday string, base, from time.Time, interval int) time.Time {
	tod := base // carries hour/min/sec/loc

	days := parseByDay(byday)
	if len(days) == 0 {
		// No BYDAY: repeat on the same weekday as base, every `interval` weeks.
		next := base
		for !next.After(from) {
			next = next.AddDate(0, 0, 7*interval)
		}
		return next
	}

	// Search up to 8 weeks ahead for the next matching weekday after `from`.
	start := time.Date(from.Year(), from.Month(), from.Day(),
		tod.Hour(), tod.Minute(), tod.Second(), 0, tod.Location())
	for offset := 0; offset < 7*8; offset++ {
		cand := start.AddDate(0, 0, offset)
		if !cand.After(from) {
			continue
		}
		if _, ok := days[cand.Weekday()]; ok {
			return cand
		}
	}
	return time.Time{}
}

// parseRule splits "KEY=VAL;KEY2=VAL2" into a map (upper-cased keys).
func parseRule(rule string) map[string]string {
	out := map[string]string{}
	for _, seg := range strings.Split(rule, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		kv := strings.SplitN(seg, "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.ToUpper(strings.TrimSpace(kv[0]))] = strings.TrimSpace(kv[1])
	}
	return out
}

var bydayMap = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday,
	"WE": time.Wednesday, "TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

func parseByDay(byday string) map[time.Weekday]struct{} {
	out := map[time.Weekday]struct{}{}
	for _, tok := range strings.Split(byday, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		if wd, ok := bydayMap[tok]; ok {
			out[wd] = struct{}{}
		}
	}
	return out
}

// PrevOccurrence returns the most recent scheduled instant at or before `now`
// (UTC) for an RRULE-lite rule fired daily at atMinuteUTC (minutes past UTC
// midnight), searching back up to 7 days. Supports FREQ=DAILY and
// FREQ=WEEKLY;BYDAY=... (the cadences a recurring "check-in" needs). Returns
// false when the rule is unsupported/empty or no occurrence falls in the
// window. Pure + deterministic.
func PrevOccurrence(rule string, atMinuteUTC int, now time.Time) (time.Time, bool) {
	parts := parseRule(rule)
	freq := strings.ToUpper(parts["FREQ"])
	if freq != "DAILY" && freq != "WEEKLY" {
		return time.Time{}, false
	}
	if atMinuteUTC < 0 {
		atMinuteUTC = 0
	}
	if atMinuteUTC > 1439 {
		atMinuteUTC = 1439
	}
	days := parseByDay(parts["BYDAY"]) // empty → every day (for WEEKLY too)
	now = now.UTC()

	for back := 0; back <= 7; back++ {
		day := now.AddDate(0, 0, -back)
		occ := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).
			Add(time.Duration(atMinuteUTC) * time.Minute)
		if occ.After(now) {
			continue // today's fire time hasn't passed yet; look back further
		}
		allowed := freq == "DAILY" || len(days) == 0
		if !allowed {
			_, allowed = days[occ.Weekday()]
		}
		if allowed {
			return occ, true
		}
	}
	return time.Time{}, false
}

// DueForSchedule reports whether a recurring task fired at atMinuteUTC under the
// given rule is due at `now`: true when the most recent scheduled occurrence is
// strictly after the last run (so each occurrence fires exactly once), or when
// there has been no run yet and an occurrence has already passed. Pure.
func DueForSchedule(rule string, atMinuteUTC int, lastRun *time.Time, now time.Time) bool {
	occ, ok := PrevOccurrence(rule, atMinuteUTC, now)
	if !ok {
		return false
	}
	if lastRun == nil {
		return true
	}
	return lastRun.UTC().Before(occ)
}

// ValidRecurrence reports whether a rule is one a routine can actually fire:
// a fixed-time cadence the daily scheduler handles (FREQ=DAILY or FREQ=WEEKLY
// with an optional BYDAY list), or an interval cadence (FREQ=HOURLY, optionally
// INTERVAL=N) fired relative to the last run. Used to validate a routine's
// cadence at creation time so a user is never handed a rule that would silently
// never run. Pure.
func ValidRecurrence(rule string) bool {
	freq := strings.ToUpper(parseRule(rule)["FREQ"])
	if freq == "DAILY" || freq == "WEEKLY" {
		return true
	}
	_, ok := IntervalMinutesForRule(rule)
	return ok
}

// minIntervalMinutes is the floor for an interval cadence, so a routine can
// never be scheduled tighter than hourly (the dispatcher ticks every minute;
// an hourly floor keeps a recurring run from hammering the model/budget).
const minIntervalMinutes = 60

// IntervalMinutesForRule returns the fixed interval (in minutes) for an
// interval-cadence rule and true, or (0,false) for a fixed-time cadence
// (DAILY/WEEKLY) or an unsupported rule. Supports FREQ=HOURLY with an optional
// INTERVAL=N (N hours, default 1), clamped to a sane floor. Pure.
func IntervalMinutesForRule(rule string) (int, bool) {
	parts := parseRule(rule)
	if strings.ToUpper(parts["FREQ"]) != "HOURLY" {
		return 0, false
	}
	hours := 1
	if v, ok := parts["INTERVAL"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	mins := hours * 60
	if mins < minIntervalMinutes {
		mins = minIntervalMinutes
	}
	return mins, true
}

// DueForInterval reports whether an interval-cadence routine is due at `now`:
// true when it has never run, or when at least intervalMinutes have elapsed
// since the last run. Pure.
func DueForInterval(intervalMinutes int, lastRun *time.Time, now time.Time) bool {
	if intervalMinutes <= 0 {
		return false
	}
	if lastRun == nil {
		return true
	}
	return !now.Before(lastRun.Add(time.Duration(intervalMinutes) * time.Minute))
}
