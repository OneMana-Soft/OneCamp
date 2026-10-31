package Calendar

import (
	"testing"
	"time"
)

func span(kind string, from, to time.Time) BusySpan {
	return BusySpan{Interval: Interval{Start: from, End: to}, Kind: kind}
}

// Calendar A3: "in a meeting" until the meetings running, and those right
// after them, end; never a title.
func TestInAMeetingRunsToTheEndOfBackToBackMeetings(t *testing.T) {
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	h := func(n float64) time.Time { return at.Add(time.Duration(n * float64(time.Hour))) }
	spans := []BusySpan{span(BusyMeeting, h(-1), h(1)), span(BusyGoogle, h(1), h(2)), span(BusyMeeting, h(3), h(4)), span(BusyFocus, h(-1), h(5))}
	st, ok := busyStatus(spans, at)
	if !ok || st.Kind != StatusBusy || !st.Until.Equal(h(2)) {
		t.Errorf("%+v %v", st, ok)
	}
	if _, ok := busyStatus(spans, h(2.5)); ok {
		t.Error("free between meetings (focus is told by the day-0 rule)")
	}
}

func TestAStatusIsSaidInWords(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, loc) // a Monday
	at := func(m time.Month, d, hh, mm int) *time.Time { t := time.Date(2026, m, d, hh, mm, 0, 0, loc); return &t }
	for _, c := range []struct {
		st     Status
		twelve bool
		want   string
	}{
		{Status{Kind: StatusBusy, Until: at(10, 19, 15, 0)}, false, "In a meeting until 15:00"},
		{Status{Kind: StatusFocus, Until: at(10, 19, 12, 30)}, true, "In focus until 12:30 PM"},
		{Status{Kind: StatusAway, Until: at(10, 20, 9, 0)}, false, "Away until Tuesday"},
		{Status{Kind: StatusAway, Until: at(10, 26, 0, 0)}, false, "Away until 26 Oct"},
		{Status{Kind: StatusAway, Until: at(10, 24, 0, 0)}, false, "Away until Saturday"},
		{Status{Kind: StatusBusy, Until: at(10, 20, 1, 0)}, false, "In a meeting until tomorrow 01:00"},
		{Status{}, false, ""},
	} {
		if got := StatusWords(c.st, now, loc, c.twelve); got != c.want {
			t.Errorf("%+v: %q, want %q", c.st, got, c.want)
		}
	}
}
