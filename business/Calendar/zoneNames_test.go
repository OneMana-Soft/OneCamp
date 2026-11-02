package Calendar

import (
	"slices"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// V-135: a zone is one zone by any of its names. The web sends a zone picked
// again under another of its names ("Asia/Kolkata" for the "Asia/Calcutta"
// Chromium saved, "Europe/Kiev" for "Europe/Kyiv"), and the server decides
// whether the time moved: it didn't, so guests aren't told it did, and an
// instance doesn't count its time as changed by hand (which would hold it
// back from the series' next change of time).
func TestAZonesOtherNameMovesNoTime(t *testing.T) {
	start, end := time.Date(2026, 10, 16, 3, 45, 0, 0, time.UTC), time.Date(2026, 10, 16, 4, 45, 0, 0, time.UTC)
	timed := func(tz, endTZ string) *models.Event {
		e := blank(tz)
		e.StartAt, e.EndAt, e.EndTZ, e.Title = &start, &end, endTZ, "Design crit"
		return e
	}
	for _, c := range []struct{ was, next [2]string }{
		{[2]string{"Asia/Kolkata", ""}, [2]string{"Asia/Calcutta", ""}},
		{[2]string{"Europe/Kyiv", ""}, [2]string{"Europe/Kiev", ""}},
		{[2]string{"America/New_York", ""}, [2]string{"US/Eastern", ""}},
		{[2]string{"UTC", ""}, [2]string{"Etc/UTC", ""}},
		// An end in the start's own zone under its other name ends where it starts.
		{[2]string{"Asia/Kolkata", ""}, [2]string{"Asia/Kolkata", "Asia/Calcutta"}},
		{[2]string{"Asia/Kolkata", "Europe/Kyiv"}, [2]string{"Asia/Kolkata", "Europe/Kiev"}},
	} {
		was, next := timed(c.was[0], c.was[1]), timed(c.next[0], c.next[1])
		if got := changedFields(was, next, false); len(got) != 0 {
			t.Errorf("%v to %v: guests are told %v changed", c.was, c.next, got)
		}
		if got := changesOf(was, next); slices.Contains(got, "time") {
			t.Errorf("%v to %v: an instance counts its time as changed (%v)", c.was, c.next, got)
		}
	}

	// Another zone is a change of time, and so is one whose clock has agreed
	// with this one's since 1970 under names of its own: Dublin keeps Irish
	// Standard Time where London keeps British Summer Time.
	for _, c := range []struct{ was, next [2]string }{
		{[2]string{"Asia/Kolkata", ""}, [2]string{"Asia/Colombo", ""}},
		{[2]string{"Europe/London", ""}, [2]string{"Europe/Dublin", ""}},
		{[2]string{"Europe/London", ""}, [2]string{"Europe/London", "Europe/Dublin"}},
		{[2]string{"UTC", ""}, [2]string{"Africa/Abidjan", ""}},
	} {
		was, next := timed(c.was[0], c.was[1]), timed(c.next[0], c.next[1])
		if got := changedFields(was, next, false); !slices.Equal(got, []string{"time"}) {
			t.Errorf("%v to %v: guests are told %v, not that the time changed", c.was, c.next, got)
		}
		if c.was[0] != c.next[0] && !slices.Contains(changesOf(was, next), "time") {
			t.Errorf("%v to %v: an instance doesn't count its time as changed", c.was, c.next)
		}
	}
}
