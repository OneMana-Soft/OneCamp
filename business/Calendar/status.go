package Calendar

// Statuses grows from day 0 (contract.go): away and focus time as before, and
// now in a meeting (StatusBusy, calendar A3), from the same busy read Find a
// time uses, never a title. StatusWords says a status: "In a meeting until
// 15:00", "In focus until 12:00", "Away until Monday".

import (
	"context"
	"time"
)

// busyStatus is "in a meeting" at at: until the meetings running then, and
// those that follow them without a gap, end. False when none runs. Pure.
func busyStatus(spans []BusySpan, at time.Time) (Status, bool) {
	var until time.Time
	for grew, edge := true, at; grew; {
		grew = false
		for _, s := range spans {
			if (s.Kind == BusyMeeting || s.Kind == BusyGoogle) && !s.Start.After(edge) && s.End.After(edge) && s.End.After(until) {
				until, grew = s.End, true
			}
		}
		edge = until
	}
	if until.IsZero() {
		return Status{}, false
	}
	return Status{Kind: StatusBusy, Until: &until}, true
}

// StatusWords says a status in words, the time on a 12-hour clock when asked:
// a later day is its weekday, a week or more its date. "" for no status. Pure.
func StatusWords(st Status, now time.Time, loc *time.Location, twelve bool) string {
	if st.Kind == "" || st.Until == nil {
		return ""
	}
	until, n := st.Until.In(loc), now.In(loc)
	when := until.Format("15:04")
	if twelve {
		when = until.Format("3:04 PM")
	}
	day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc) }
	days := int(day(until).Sub(day(n)).Hours()/24 + 0.5)
	switch {
	case days >= 7:
		when = until.Format("2 Jan")
	case days >= 2 || (days == 1 && st.Kind == StatusAway):
		when = until.Format("Monday")
	case days == 1:
		when = "tomorrow " + when
	}
	switch st.Kind {
	case StatusAway:
		return "Away until " + when
	case StatusFocus:
		return "In focus until " + when
	default:
		return "In a meeting until " + when
	}
}

// meetingStatus is the day-0 answer, then a meeting when it says nothing.
func meetingStatus(ctx context.Context, personUUID string, at time.Time, dayZero Status) (Status, error) {
	if dayZero.Kind != "" {
		return dayZero, nil
	}
	b, err := ReadBusy(ctx, personUUID, at, at.Add(24*time.Hour), BusyOptions{})
	if err != nil {
		return dayZero, err
	}
	st, _ := busyStatus(b.Spans, at)
	return st, nil
}

// StatusSeenBy is what personUUID's calendar says of them at at, as askerUUID
// may see it: a status tells when someone is busy, so it follows the one rule
// (PeopleSeenBy). Private says the workspace keeps it from the asker, and
// then nothing of theirs is read.
func StatusSeenBy(ctx context.Context, askerUUID, personUUID string, at time.Time) (st Status, private bool, err error) {
	seen, _, err := PeopleSeenBy(ctx, askerUUID, []string{personUUID})
	if err != nil || len(seen) == 0 {
		return Status{}, err == nil, err
	}
	st, err = Statuses.StatusFor(ctx, personUUID, at)
	return st, false, err
}
