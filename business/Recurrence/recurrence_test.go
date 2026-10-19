package business

import (
	"errors"
	"strings"
	"testing"
	"time"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	"github.com/akashc777/OneCamp/tests/timing"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func mustRule(t *testing.T, text string) Rule {
	t.Helper()
	r, err := Parse(text)
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return r
}

// days writes instants as their local dates, "1997-09-02 …", for comparing.
func days(ts []time.Time, loc *time.Location, layout string) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.In(loc).Format(layout))
	}
	return strings.Join(out, " ")
}

// RFC 5545 §3.8.5.3's own examples, every one of the subset events use, in
// the RFC's zone (America/New_York): the dates and the 09:00 wall clock,
// whichever side of a clock change they fall on.
func TestTheRFCsOwnExamples(t *testing.T) {
	ny := zone(t, "America/New_York")
	at := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 9, 0, 0, 0, ny) }
	for _, c := range []struct {
		name, rule string
		start      time.Time
		want       string // local dates, space-separated
		count      int    // when the list is long: how many, with want the first ones
		ex         []time.Time
	}{
		{"daily for 10", "FREQ=DAILY;COUNT=10", at(1997, 9, 2),
			"09-02 09-03 09-04 09-05 09-06 09-07 09-08 09-09 09-10 09-11", 0, nil},
		{"daily until 24 December", "FREQ=DAILY;UNTIL=19971224T000000Z", at(1997, 9, 2), "09-02 09-03 09-04", 113, nil},
		{"every other day", "FREQ=DAILY;INTERVAL=2", at(1997, 9, 2), "09-02 09-04 09-06 09-08", 0, nil},
		{"every 10 days, 5 times", "FREQ=DAILY;INTERVAL=10;COUNT=5", at(1997, 9, 2), "09-02 09-12 09-22 10-02 10-12", 0, nil},
		{"every day in January for 3 years", "FREQ=YEARLY;UNTIL=20000131T140000Z;BYMONTH=1;BYDAY=SU,MO,TU,WE,TH,FR,SA", at(1998, 1, 1), "01-01 01-02 01-03", 93, nil},
		{"the same, daily", "FREQ=DAILY;UNTIL=20000131T140000Z;BYMONTH=1", at(1998, 1, 1), "01-01 01-02 01-03", 93, nil},
		{"weekly for 10", "FREQ=WEEKLY;COUNT=10", at(1997, 9, 2),
			"09-02 09-09 09-16 09-23 09-30 10-07 10-14 10-21 10-28 11-04", 0, nil},
		{"every other week on Monday, Wednesday and Friday", "FREQ=WEEKLY;INTERVAL=2;UNTIL=19971224T000000Z;WKST=SU;BYDAY=MO,WE,FR", at(1997, 9, 1),
			"09-01 09-03 09-05 09-15 09-17 09-19 09-29 10-01 10-03 10-13 10-15 10-17 10-27 10-29 10-31 11-10 11-12 11-14 11-24 11-26 11-28 12-08 12-10 12-12 12-22", 0, nil},
		{"the first Friday, 10 times", "FREQ=MONTHLY;COUNT=10;BYDAY=1FR", at(1997, 9, 5),
			"09-05 10-03 11-07 12-05 01-02 02-06 03-06 04-03 05-01 06-05", 0, nil},
		{"every other month, first and last Sunday", "FREQ=MONTHLY;INTERVAL=2;COUNT=10;BYDAY=1SU,-1SU", at(1997, 9, 7),
			"09-07 09-28 11-02 11-30 01-04 01-25 03-01 03-29 05-03 05-31", 0, nil},
		{"the second to last Monday, 6 times", "FREQ=MONTHLY;COUNT=6;BYDAY=-2MO", at(1997, 9, 22),
			"09-22 10-20 11-17 12-22 01-19 02-16", 0, nil},
		{"the third to last day", "FREQ=MONTHLY;BYMONTHDAY=-3", at(1997, 9, 28), "09-28 10-29 11-28 12-29 01-29 02-26", 0, nil},
		{"the 2nd and 15th, 10 times", "FREQ=MONTHLY;COUNT=10;BYMONTHDAY=2,15", at(1997, 9, 2),
			"09-02 09-15 10-02 10-15 11-02 11-15 12-02 12-15 01-02 01-15", 0, nil},
		{"the first and last day, 10 times", "FREQ=MONTHLY;COUNT=10;BYMONTHDAY=1,-1", at(1997, 9, 30),
			"09-30 10-01 10-31 11-01 11-30 12-01 12-31 01-01 01-31 02-01", 0, nil},
		{"June and July, 10 times", "FREQ=YEARLY;COUNT=10;BYMONTH=6,7", at(1997, 6, 10),
			"06-10 07-10 06-10 07-10 06-10 07-10 06-10 07-10 06-10 07-10", 0, nil},
		{"every Thursday in March", "FREQ=YEARLY;BYMONTH=3;BYDAY=TH", at(1997, 3, 13),
			"03-13 03-20 03-27 03-05 03-12 03-19 03-26 03-04 03-11 03-18 03-25", 0, nil},
		{"every Friday the 13th", "FREQ=MONTHLY;BYDAY=FR;BYMONTHDAY=13", at(1997, 9, 2),
			"02-13 03-13 11-13 08-13 10-13", 0, []time.Time{at(1997, 9, 2)}},
		{"the first Saturday after the first Sunday", "FREQ=MONTHLY;BYDAY=SA;BYMONTHDAY=7,8,9,10,11,12,13", at(1997, 9, 13),
			"09-13 10-11 11-08 12-13 01-10 02-07 03-07 04-11 05-09 06-13", 0, nil},
		{"the third of Tuesday, Wednesday or Thursday, 3 months", "FREQ=MONTHLY;COUNT=3;BYDAY=TU,WE,TH;BYSETPOS=3", at(1997, 9, 4),
			"09-04 10-07 11-06", 0, nil},
		{"the second to last weekday", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-2", at(1997, 9, 29),
			"09-29 10-30 11-27 12-30 01-29 02-26 03-30", 0, nil},
		{"an invalid date is skipped", "FREQ=MONTHLY;BYMONTHDAY=15,30;COUNT=5", time.Date(2007, 1, 15, 9, 0, 0, 0, ny),
			"01-15 01-30 02-15 03-15 03-30", 0, nil},
		{"WKST Monday", "FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=MO", at(1997, 8, 5), "08-05 08-10 08-19 08-24", 0, nil},
		{"WKST Sunday", "FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=SU", at(1997, 8, 5), "08-05 08-17 08-19 08-31", 0, nil},
	} {
		r := mustRule(t, c.rule)
		got := Expand(r, c.start, ny, c.start, c.start.AddDate(6, 0, 0), Options{Exdates: c.ex})
		want := strings.Fields(c.want)
		if c.count > 0 && len(got) != c.count {
			t.Errorf("%s: %d instances, want %d", c.name, len(got), c.count)
		}
		if len(got) < len(want) {
			t.Errorf("%s: %d instances, want at least %d: %s", c.name, len(got), len(want), days(got, ny, "01-02"))
			continue
		}
		if g := days(got[:len(want)], ny, "01-02"); g != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, g, c.want)
		}
		for _, inst := range got {
			if h, m, _ := inst.In(ny).Clock(); h != 9 || m != 0 {
				t.Errorf("%s: an instance at %v, not 09:00 New York", c.name, inst.In(ny))
				break
			}
		}
	}
}

// A series keeps its wall-clock time in its own zone across every clock
// change, in every zone of the matrix: a time a change skips moves forward by
// the gap, and one it repeats is its first.
func TestASeriesKeepsItsTimeAcrossClockChanges(t *testing.T) {
	for _, name := range []string{"UTC", "America/New_York", "Asia/Kolkata", "Europe/London", "Pacific/Chatham", "America/St_Johns"} {
		loc := zone(t, name)
		start := time.Date(2026, 1, 5, 9, 15, 0, 0, loc)
		got := Expand(mustRule(t, "FREQ=DAILY"), start, loc, start, start.AddDate(1, 0, 0), Options{})
		if len(got) != 366 {
			t.Errorf("%s: %d days, want 366", name, len(got))
		}
		for _, inst := range got {
			if h, m, _ := inst.In(loc).Clock(); h != 9 || m != 15 {
				t.Errorf("%s: %v is not 09:15 there", name, inst.In(loc))
				break
			}
		}
	}
	ny := zone(t, "America/New_York")
	start := time.Date(2026, 3, 6, 2, 30, 0, 0, ny)
	got := Expand(mustRule(t, "FREQ=DAILY;COUNT=4"), start, ny, start, start.AddDate(0, 0, 10), Options{})
	if g := days(got, ny, "01-02 15:04"); g != "03-06 02:30 03-07 02:30 03-08 03:30 03-09 02:30" {
		t.Errorf("across the gap: %s", g)
	}
	start = time.Date(2026, 10, 30, 1, 30, 0, 0, ny)
	got = Expand(mustRule(t, "FREQ=DAILY;COUNT=4"), start, ny, start, start.AddDate(0, 0, 10), Options{})
	if g := days(got, ny, "01-02 15:04 MST"); g != "10-30 01:30 EDT 10-31 01:30 EDT 11-01 01:30 EDT 11-02 01:30 EST" {
		t.Errorf("across the repeat: %s", g)
	}
	// Chatham's clocks go forward at 02:45 on the last Sunday of September.
	chatham := zone(t, "Pacific/Chatham")
	start = time.Date(2026, 9, 26, 3, 0, 0, 0, chatham)
	got = Expand(mustRule(t, "FREQ=DAILY;COUNT=2"), start, chatham, start, start.AddDate(0, 0, 3), Options{})
	if g := days(got, chatham, "01-02 15:04"); g != "09-26 03:00 09-27 04:00" {
		t.Errorf("Chatham's gap: %s", g)
	}
}

// An all-day series is dates: UTC midnights, the same day for everyone.
func TestAnAllDaySeriesIsDates(t *testing.T) {
	start := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	r := mustRule(t, "FREQ=WEEKLY;UNTIL=20261106")
	got := Expand(r, start, time.UTC, start, start.AddDate(1, 0, 0), Options{})
	if g := days(got, time.UTC, "2006-01-02T15:04"); g != "2026-10-16T00:00 2026-10-23T00:00 2026-10-30T00:00 2026-11-06T00:00" {
		t.Errorf("got %s", g)
	}
}

// With Clamp, a monthly or yearly repeat stays on its day where a month is
// too short, on the month's last day: exactly what task repeats do today
// (business/Scheduler NextRunOnDay), so they can move onto this engine with
// no change of behaviour. Without it, as RFC 5545 says, such a month has
// none.
func TestClampIsTodaysTaskRepeat(t *testing.T) {
	for _, c := range []struct {
		rule  string
		start time.Time
		day   int
	}{
		{"FREQ=MONTHLY", time.Date(2026, 1, 31, 9, 0, 0, 0, time.UTC), 31},
		{"FREQ=MONTHLY;INTERVAL=2", time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC), 31},
		{"FREQ=MONTHLY", time.Date(2026, 1, 30, 9, 0, 0, 0, time.UTC), 30},
		{"FREQ=YEARLY", time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC), 29},
	} {
		got := Expand(mustRule(t, c.rule), c.start, time.UTC, c.start, c.start.AddDate(6, 0, 0), Options{Clamp: true, Limit: 13})
		prev := c.start
		for i, inst := range got[1:] {
			want, err := schedulerBusiness.NextRunOnDay(c.rule, prev, prev, c.day)
			if err != nil || !inst.Equal(want) {
				t.Errorf("%s from %s, step %d: %v, the task repeat says %v (%v)", c.rule, c.start.Format("2 Jan 2006"), i+1, inst, want, err)
				break
			}
			prev = inst
		}
	}
	// Without Clamp: the RFC skips the months without a 31st.
	start := time.Date(2026, 1, 31, 9, 0, 0, 0, time.UTC)
	got := Expand(mustRule(t, "FREQ=MONTHLY;COUNT=4"), start, time.UTC, start, start.AddDate(2, 0, 0), Options{})
	if g := days(got, time.UTC, "01-02"); g != "01-31 03-31 05-31 07-31" {
		t.Errorf("unclamped: %s", g)
	}
}

// Rules events can't use, or that contradict themselves, are refused in
// words; every rule read is written back in one form that reads the same.
func TestARuleIsReadExactlyOrRefused(t *testing.T) {
	for _, bad := range []string{"", "FREQ=HOURLY", "FREQ=DAILY;BYWEEKNO=20", "FREQ=DAILY;COUNT=3;UNTIL=20261231",
		"FREQ=WEEKLY;BYMONTHDAY=1", "FREQ=WEEKLY;BYDAY=2TU", "FREQ=MONTHLY;BYDAY=6TU", "FREQ=DAILY;INTERVAL=0",
		"FREQ=DAILY;COUNT=1001", "FREQ=DAILY;UNTIL=tomorrow", "FREQ=DAILY;BYSETPOS=1", "FREQ=DAILY;FREQ=WEEKLY",
		"FREQ=MONTHLY;BYMONTHDAY=0", "FREQ=DAILY;X-NAME=1", "BYDAY=MO", "FREQ=DAILY;UNTIL=20261231T120000"} {
		var unread *ErrRule
		if _, err := Parse(bad); !errors.As(err, &unread) {
			t.Errorf("%q read as a rule (%v)", bad, err)
		}
	}
	for in, want := range map[string]string{
		"RRULE:freq=weekly;byday=th,tu":                 "FREQ=WEEKLY;BYDAY=TU,TH",
		"FREQ=MONTHLY;BYDAY=-1FR;INTERVAL=1":            "FREQ=MONTHLY;BYDAY=-1FR",
		"FREQ=YEARLY;BYMONTH=9,3;BYDAY=1MO;COUNT=4":     "FREQ=YEARLY;BYMONTH=3,9;BYDAY=1MO;COUNT=4",
		"FREQ=MONTHLY;BYMONTHDAY=-1,1;UNTIL=20261218":   "FREQ=MONTHLY;BYMONTHDAY=1,-1;UNTIL=20261218",
		"FREQ=WEEKLY;WKST=SU;BYDAY=SU,MO;INTERVAL=2":    "FREQ=WEEKLY;INTERVAL=2;BYDAY=SU,MO;WKST=SU",
		"FREQ=DAILY;UNTIL=20261231T235959Z":             "FREQ=DAILY;UNTIL=20261231T235959Z",
		"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1": "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1",
	} {
		r := mustRule(t, in)
		if got := r.String(); got != want {
			t.Errorf("%q is written %q, want %q", in, got, want)
		}
		if again := mustRule(t, r.String()).String(); again != want {
			t.Errorf("%q doesn't read back as itself: %q", want, again)
		}
	}
}

// A rule in words, from its start, as the repeat menu says its presets, and
// a sentence for the rest.
func TestARuleInWords(t *testing.T) {
	london := zone(t, "Europe/London")
	tue := time.Date(2026, 10, 13, 10, 0, 0, 0, london) // the second Tuesday of October
	for rule, want := range map[string]string{
		"FREQ=DAILY":              "Daily",
		"FREQ=DAILY;INTERVAL=3":   "Every 3 days",
		"FREQ=WEEKLY":             "Weekly on Tuesday",
		"FREQ=WEEKLY;BYDAY=TU,TH": "Weekly on Tuesday and Thursday",
		"FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,TH;UNTIL=20261218T235959Z": "Every 2 weeks on Tuesday and Thursday, until 18 December 2026",
		"FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR":                          "Every weekday",
		"FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR,SA,SU":                    "Daily",
		"FREQ=MONTHLY":                                              "Monthly on day 13",
		"FREQ=MONTHLY;BYDAY=2TU":                                    "Monthly on the second Tuesday",
		"FREQ=MONTHLY;BYDAY=-1FR;COUNT=6":                           "Monthly on the last Friday, 6 times",
		"FREQ=MONTHLY;BYMONTHDAY=-1":                                "Monthly on the last day",
		"FREQ=MONTHLY;BYMONTHDAY=1,15":                              "Monthly on days 1 and 15",
		"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1":             "Monthly on the last weekday",
		"FREQ=MONTHLY;INTERVAL=3;BYDAY=1MO":                         "Every 3 months on the first Monday",
		"FREQ=YEARLY":                                               "Annually on 13 October",
		"FREQ=YEARLY;BYMONTH=3;BYDAY=TH":                            "Annually on every Thursday of March",
		"FREQ=YEARLY;BYMONTH=3,9;BYDAY=1MO":                         "Annually on the first Monday of March and September",
		"FREQ=DAILY;COUNT=1":                                        "Daily, once",
		"FREQ=WEEKLY;UNTIL=20261218":                                "Weekly on Tuesday, until 18 December 2026",
	} {
		if got := Describe(mustRule(t, rule), tue, london); got != want {
			t.Errorf("%s: %q, want %q", rule, got, want)
		}
	}
}

// "This and following" cuts a series in two: the first part ends before the
// instance, the rest starts there, and a counted series shares its count.
func TestASeriesCutsInTwo(t *testing.T) {
	london := zone(t, "Europe/London")
	start := time.Date(2026, 10, 6, 10, 0, 0, 0, london)
	at := time.Date(2026, 10, 27, 10, 0, 0, 0, london)
	counted := mustRule(t, "FREQ=WEEKLY;COUNT=10")
	before, rest := Before(counted, start, london, at, false), From(counted, start, london, at)
	if before.Count != 3 || rest.Count != 7 {
		t.Errorf("counted: %d and %d", before.Count, rest.Count)
	}
	open := mustRule(t, "FREQ=WEEKLY")
	b := Before(open, start, london, at, false)
	if got := Expand(b, start, london, start, at.AddDate(1, 0, 0), Options{}); len(got) != 3 || !got[2].Before(at) {
		t.Errorf("open, cut: %v", got)
	}
	day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	allDay := Before(open, day, time.UTC, day.AddDate(0, 0, 21), true)
	if !allDay.UntilDate || allDay.Until.Format("20060102") != "20261026" {
		t.Errorf("all day, cut: %s", allDay)
	}
}

// A series moved by a day takes its weekdays and month days with it.
func TestAMovedSeriesTakesItsDaysAlong(t *testing.T) {
	cases := []struct {
		rule, want string
		days       int
	}{
		{"FREQ=WEEKLY;BYDAY=TU", "FREQ=WEEKLY;BYDAY=WE", 1},
		{"FREQ=WEEKLY;BYDAY=MO,FR", "FREQ=WEEKLY;BYDAY=TH,SU", -1},
		{"FREQ=WEEKLY;BYDAY=SA", "FREQ=WEEKLY;BYDAY=MO", 2},
		{"FREQ=MONTHLY;BYDAY=2TU", "FREQ=MONTHLY;BYDAY=2WE", 1},
		{"FREQ=MONTHLY;BYMONTHDAY=31", "FREQ=MONTHLY;BYMONTHDAY=1", 1},
		{"FREQ=MONTHLY;BYMONTHDAY=-1", "FREQ=MONTHLY;BYMONTHDAY=-1", 1},
		{"FREQ=DAILY", "FREQ=DAILY", 3},
		{"FREQ=WEEKLY;BYDAY=TU", "FREQ=WEEKLY;BYDAY=TU", 7},
	}
	for _, c := range cases {
		if got := Shift(mustRule(t, c.rule), c.days).String(); got != c.want {
			t.Errorf("%s moved %d days: %s, want %s", c.rule, c.days, got, c.want)
		}
	}
}

// Next is the first instance after a moment; Last is a series' last start,
// none for one that never ends.
func TestNextAndLast(t *testing.T) {
	start := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	r := mustRule(t, "FREQ=DAILY;COUNT=3")
	if n, ok := Next(r, start, time.UTC, start, Options{}); !ok || !n.Equal(start.AddDate(0, 0, 1)) {
		t.Errorf("next: %v %v", n, ok)
	}
	if _, ok := Next(r, start, time.UTC, start.AddDate(0, 0, 2), Options{}); ok {
		t.Error("a next after the last")
	}
	if l, ok := Last(r, start, time.UTC, Options{}); !ok || !l.Equal(start.AddDate(0, 0, 2)) {
		t.Errorf("last: %v %v", l, ok)
	}
	if _, ok := Last(mustRule(t, "FREQ=DAILY"), start, time.UTC, Options{}); ok {
		t.Error("an endless series has a last")
	}
	// An old series read far from its start: the same instances as reading
	// from its start, without walking every day since.
	old := time.Date(2010, 3, 1, 9, 0, 0, 0, time.UTC)
	w := mustRule(t, "FREQ=WEEKLY;INTERVAL=3;BYDAY=MO,TH")
	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	all := Expand(w, old, time.UTC, old, to, Options{Limit: 100000})
	var want []time.Time
	for _, t := range all {
		if !t.Before(from) {
			want = append(want, t)
		}
	}
	if got := Expand(w, old, time.UTC, from, to, Options{}); days(got, time.UTC, "2006-01-02") != days(want, time.UTC, "2006-01-02") {
		t.Errorf("read near the window: %s, want %s", days(got, time.UTC, "2006-01-02"), days(want, time.UTC, "2006-01-02"))
	}
}

// expansionBudget is the most CPU 10,000 instances may cost: 20 ms, or four
// times that with the race detector built in, which slows the expansion
// itself about four times (19-20 ms of CPU against 5-8 ms in a plain build,
// at a load of 54, 18 Oct 2026). At 20 ms under -race the budget failed make
// verify at random (21.95 ms at a load of 60) on nothing but the detector.
func expansionBudget() time.Duration {
	if timing.Race {
		return 80 * time.Millisecond
	}
	return 20 * time.Millisecond
}

// expandTenThousand is the least CPU time, over runs runs, that times
// expansions of 10,000 daily instances took in one run (times > 1 stands in
// for code that many times slower), and how many instances one gave.
func expandTenThousand(t *testing.T, runs, times int) (time.Duration, int) {
	t.Helper()
	london := zone(t, "Europe/London")
	start := time.Date(2026, 1, 1, 9, 15, 0, 0, london)
	r := mustRule(t, "FREQ=DAILY")
	var n int
	took := timing.FastestCPU(runs, func() {
		for i := 0; i < times; i++ {
			n = len(Expand(r, start, london, start, start.AddDate(30, 0, 0), Options{Limit: 10000}))
		}
	})
	return took, n
}

// 10,000 instances of a daily series expand within the programme's budget,
// 20 ms (PROGRAM.md A1 tests), in CPU time, the least of twenty runs: the
// fastest wall time of twenty took 24 ms at a load of 60 beside a web gate,
// which measured the machine, not the expansion.
func TestTenThousandInstancesExpandInBudget(t *testing.T) {
	took, n := expandTenThousand(t, 20, 1)
	if n != 10000 {
		t.Fatalf("%d instances", n)
	}
	t.Logf("10,000 instances: %v of CPU (budget %v)", took, expansionBudget())
	if took > expansionBudget() {
		t.Errorf("10,000 instances took %v of CPU, over the %v budget", took, expansionBudget())
	}
}

// The budget still catches a real slowdown: ten expansions in one timed run,
// as code ten times slower would take, are over it, race detector or not.
func TestTheExpansionBudgetCatchesCodeTenTimesSlower(t *testing.T) {
	took, _ := expandTenThousand(t, 3, 10)
	t.Logf("ten expansions: %v of CPU (budget %v)", took, expansionBudget())
	if took <= expansionBudget() {
		t.Errorf("ten expansions took %v of CPU, within the %v budget: it would miss code ten times slower", took, expansionBudget())
	}
}
