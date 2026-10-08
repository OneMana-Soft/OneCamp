package business

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	q, s, err := Check(Input{Question: "  What did you   work on today? ", Days: []int{1, 2, 3, 4, 5}, Time: "17:00", TZ: "Asia/Kolkata"})
	if err != nil || q != "What did you work on today?" || s.Days != 31 || s.AtMinute != 17*60 || s.Loc.String() != "Asia/Kolkata" {
		t.Fatalf("got %q %+v %v", q, s, err)
	}
	for want, in := range map[string]Input{
		"Write the question":    {Days: []int{1}, Time: "09:00"},
		"under 300":             {Question: strings.Repeat("a", 301), Days: []int{1}, Time: "09:00"},
		"at least one day":      {Question: "Q", Time: "09:00"},
		"from Monday to Sunday": {Question: "Q", Days: []int{8}, Time: "09:00"},
		"Choose the time":       {Question: "Q", Days: []int{1}, Time: "25:00"},
		"time zone isn't one":   {Question: "Q", Days: []int{1}, Time: "09:00", TZ: "Mars/Olympus"},
	} {
		var ce *CheckInError
		if _, _, err := Check(in); !errors.As(err, &ce) || !strings.Contains(ce.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	if _, s, err := Check(Input{Question: "Q", Days: []int{7}, Time: "09:00"}); err != nil || s.Loc.String() != "UTC" || s.Days != 64 {
		t.Errorf("no zone is UTC, Sunday is the last bit: %+v %v", s, err)
	}
}

func TestNext(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	weekdays5pm := Schedule{Days: 31, AtMinute: 17 * 60, Loc: ny}
	at := func(s Schedule, after time.Time) time.Time {
		t.Helper()
		got, err := Next(s, after)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Friday 6 Nov 2026, 18:00 New York: past today's, so Monday 9 Nov, 17:00.
	if got := at(weekdays5pm, time.Date(2026, 11, 6, 18, 0, 0, 0, ny)); !got.Equal(time.Date(2026, 11, 9, 17, 0, 0, 0, ny)) {
		t.Errorf("after Friday's, Monday's: %v", got)
	}
	// Before today's time, today's; exactly at it, the next.
	if got := at(weekdays5pm, time.Date(2026, 11, 6, 9, 0, 0, 0, ny)); !got.Equal(time.Date(2026, 11, 6, 17, 0, 0, 0, ny)) {
		t.Errorf("before it, today's: %v", got)
	}
	if got := at(weekdays5pm, time.Date(2026, 11, 6, 17, 0, 0, 0, ny)); !got.Equal(time.Date(2026, 11, 9, 17, 0, 0, 0, ny)) {
		t.Errorf("at the time, the next: %v", got)
	}
	// Across the clocks going back (1 Nov 2026), 17:00 stays 17:00 local.
	if got := at(weekdays5pm, time.Date(2026, 10, 30, 18, 0, 0, 0, ny)); got.In(ny).Hour() != 17 || got.Weekday() != time.Monday {
		t.Errorf("17:00 New York across DST: %v", got.In(ny))
	}
	// A time the clocks skip (02:30 on 8 Mar 2026 in New York) is asked at the
	// first minute after the gap, 03:00, never before it.
	sundays230 := Schedule{Days: 64, AtMinute: 2*60 + 30, Loc: ny}
	if got := at(sundays230, time.Date(2026, 3, 7, 12, 0, 0, 0, ny)); got.In(ny).Hour() != 3 || got.In(ny).Minute() != 0 || got.In(ny).Day() != 8 {
		t.Errorf("a skipped 02:30 is asked at 03:00 that day: %v", got.In(ny))
	}
	// Santiago's clocks spring forward at midnight (6 Sep 2026): a Sundays-only
	// 00:30 check-in still asks on that Sunday, not never and not on Saturday.
	scl, _ := time.LoadLocation("America/Santiago")
	sundays0030 := Schedule{Days: 64, AtMinute: 30, Loc: scl}
	if got := at(sundays0030, time.Date(2026, 9, 5, 12, 0, 0, 0, scl)); got.In(scl).Weekday() != time.Sunday || got.In(scl).Day() != 6 {
		t.Errorf("Santiago's Sunday at its midnight change: %v", got.In(scl))
	}
	weekend0030 := Schedule{Days: 32 | 64, AtMinute: 30, Loc: scl}
	sat := at(weekend0030, time.Date(2026, 9, 5, 0, 10, 0, 0, scl))
	sun := at(weekend0030, sat)
	if sat.In(scl).Weekday() != time.Saturday || sun.In(scl).Weekday() != time.Sunday {
		t.Errorf("Saturday's then Sunday's, each on its own day: %v, %v", sat.In(scl), sun.In(scl))
	}
	// Mondays only, asked from a Tuesday: next Monday.
	mondays := Schedule{Days: 1, AtMinute: 9*60 + 30, Loc: time.UTC}
	if got := at(mondays, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("Mondays: %v", got)
	}
	if _, err := Next(Schedule{Days: 0, Loc: time.UTC}, time.Now()); err == nil {
		t.Errorf("a schedule with no days has no next time")
	}
}

func TestQuestionHTML(t *testing.T) {
	got := QuestionHTML("What <b>blocked</b> you?", time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC))
	if !strings.Contains(got, "What &lt;b&gt;blocked&lt;/b&gt; you?") || !strings.Contains(got, "Thursday 8 October") || !strings.Contains(got, "Answer in this thread") {
		t.Errorf("got %s", got)
	}
}
