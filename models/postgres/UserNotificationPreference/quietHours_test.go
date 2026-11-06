package models

import (
	"strconv"
	"strings"
	"testing"
	"time"

	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
)

// week is the same ranges ("09:00-18:00") on each of days (0 Sunday).
func week(days []int, ranges ...string) map[string][]weekHours.Span {
	out := map[string][]weekHours.Span{}
	for _, d := range days {
		for _, r := range ranges {
			a, b, _ := strings.Cut(r, "-")
			out[strconv.Itoa(d)] = append(out[strconv.Itoa(d)], weekHours.Span{Start: a, End: b})
		}
	}
	return out
}

var (
	weekdays = []int{1, 2, 3, 4, 5}
	everyDay = []int{0, 1, 2, 3, 4, 5, 6}
	sunday   = []int{0}
)

// scheduleWeeks are the weeks the hold is tried with, by name; each is tried
// in Asia/Kolkata (no daylight saving) and in America/New_York across both of
// 2026's changes (8 March, 02:00 EST to 03:00 EDT; 1 November, 02:00 EDT back
// to 01:00 EST). The last three sit where those changes move the clock.
var scheduleWeeks = map[string]map[string][]weekHours.Span{
	"weekdays only":             week(weekdays, "09:00-18:00"),
	"weekend off":               week(weekdays, "00:00-24:00"),
	"two ranges a day":          week(weekdays, "09:00-12:30", "13:30-17:00"),
	"evenings to midnight":      week(everyDay, "18:00-24:00"),
	"starts in spring's gap":    week(sunday, "02:30-05:00"),
	"inside spring's gap":       week(sunday, "02:00-02:30"),
	"in autumn's repeated hour": week(sunday, "01:30-01:45"),
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// The hold, case by case: outside the schedule's ranges notifications are
// held until the next range starts, by the wall clock in its zone, and inside
// them they aren't. A schedule decides by itself; with none, the daily window
// decides as it always did.
func TestQuietHoursUntilReadsTheSchedule(t *testing.T) {
	ist, nyc := zone(t, "Asia/Kolkata"), zone(t, "America/New_York")
	in := func(loc *time.Location, mo time.Month, d, h, m, s int) time.Time {
		return time.Date(2026, mo, d, h, m, s, 0, loc)
	}
	utc := func(mo time.Month, d, h, m int) time.Time { return time.Date(2026, mo, d, h, m, 0, 0, time.UTC) }
	str := func(s string) *string { return &s }
	sched := func(name, tz string) *Schedule { return &Schedule{TZ: tz, Week: scheduleWeeks[name]} }
	const kolkata, newYork = "Asia/Kolkata", "America/New_York"
	cases := []struct {
		name  string
		pref  UserNotificationPreference
		now   time.Time
		held  bool
		until time.Time
	}{
		// 2 November 2026 is a Monday.
		{"weekdays: inside", UserNotificationPreference{Schedule: sched("weekdays only", kolkata)}, in(ist, 11, 4, 10, 0, 0), false, time.Time{}},
		{"weekdays: a minute before nine", UserNotificationPreference{Schedule: sched("weekdays only", kolkata)}, in(ist, 11, 4, 8, 59, 0), true, in(ist, 11, 4, 9, 0, 0)},
		{"weekdays: the evening", UserNotificationPreference{Schedule: sched("weekdays only", kolkata)}, in(ist, 11, 4, 18, 0, 0), true, in(ist, 11, 5, 9, 0, 0)},
		{"weekdays: Friday evening waits for Monday", UserNotificationPreference{Schedule: sched("weekdays only", kolkata)}, in(ist, 11, 6, 18, 30, 0), true, in(ist, 11, 9, 9, 0, 0)},
		{"weekdays: Saturday", UserNotificationPreference{Schedule: sched("weekdays only", kolkata)}, in(ist, 11, 7, 12, 0, 0), true, in(ist, 11, 9, 9, 0, 0)},
		{"weekend off: Friday's last second", UserNotificationPreference{Schedule: sched("weekend off", kolkata)}, in(ist, 11, 6, 23, 59, 59), false, time.Time{}},
		{"weekend off: Saturday's first", UserNotificationPreference{Schedule: sched("weekend off", kolkata)}, in(ist, 11, 7, 0, 0, 0), true, in(ist, 11, 9, 0, 0, 0)},
		{"weekend off: Sunday's last second", UserNotificationPreference{Schedule: sched("weekend off", kolkata)}, in(ist, 11, 8, 23, 59, 59), true, in(ist, 11, 9, 0, 0, 0)},
		{"weekend off: Monday", UserNotificationPreference{Schedule: sched("weekend off", kolkata)}, in(ist, 11, 9, 0, 0, 0), false, time.Time{}},
		{"two ranges: the morning's last second", UserNotificationPreference{Schedule: sched("two ranges a day", kolkata)}, in(ist, 11, 3, 12, 29, 59), false, time.Time{}},
		{"two ranges: lunch", UserNotificationPreference{Schedule: sched("two ranges a day", kolkata)}, in(ist, 11, 3, 12, 30, 0), true, in(ist, 11, 3, 13, 30, 0)},
		{"two ranges: after lunch", UserNotificationPreference{Schedule: sched("two ranges a day", kolkata)}, in(ist, 11, 3, 13, 30, 0), false, time.Time{}},
		{"two ranges: the day's end", UserNotificationPreference{Schedule: sched("two ranges a day", kolkata)}, in(ist, 11, 3, 17, 0, 0), true, in(ist, 11, 4, 9, 0, 0)},
		{"to 24:00: the day's last second", UserNotificationPreference{Schedule: sched("evenings to midnight", kolkata)}, in(ist, 11, 3, 23, 59, 59), false, time.Time{}},
		{"to 24:00: midnight", UserNotificationPreference{Schedule: sched("evenings to midnight", kolkata)}, in(ist, 11, 4, 0, 0, 0), true, in(ist, 11, 4, 18, 0, 0)},
		{"to 24:00: a minute before six", UserNotificationPreference{Schedule: sched("evenings to midnight", kolkata)}, in(ist, 11, 4, 17, 59, 0), true, in(ist, 11, 4, 18, 0, 0)},

		// New York: nine on Monday is 13:00 UTC after the spring change, 14:00 after the autumn one.
		{"New York: Friday before spring's change", UserNotificationPreference{Schedule: sched("weekdays only", newYork)}, in(nyc, 3, 6, 18, 0, 0), true, utc(3, 9, 13, 0)},
		{"New York: Friday before autumn's change", UserNotificationPreference{Schedule: sched("weekdays only", newYork)}, in(nyc, 10, 30, 18, 0, 0), true, utc(11, 2, 14, 0)},
		{"New York: Monday nine after spring's change", UserNotificationPreference{Schedule: sched("weekdays only", newYork)}, utc(3, 9, 13, 0), false, time.Time{}},
		{"New York: the second before", UserNotificationPreference{Schedule: sched("weekdays only", newYork)}, utc(3, 9, 13, 0).Add(-time.Second), true, utc(3, 9, 13, 0)},
		// 02:00 EST is 03:00 EDT: the clock jumps into 02:30-05:00 at 07:00 UTC.
		{"spring: the clock jumps into the range", UserNotificationPreference{Schedule: sched("starts in spring's gap", newYork)}, utc(3, 8, 6, 0), true, utc(3, 8, 7, 0)},
		{"spring: inside after the jump", UserNotificationPreference{Schedule: sched("starts in spring's gap", newYork)}, utc(3, 8, 7, 0), false, time.Time{}},
		// 02:00-02:30 never shows on 8 March: next Sunday's 02:00 EDT.
		{"spring: a range the clock skips", UserNotificationPreference{Schedule: sched("inside spring's gap", newYork)}, utc(3, 8, 6, 30), true, utc(3, 15, 6, 0)},
		// The skipped range's next showing is a week after the day it is skipped, so it can be
		// nearly two weeks off: asked the Thursday before, or the minute after the week before's.
		{"spring: a range the clock skips, asked the Thursday before", UserNotificationPreference{Schedule: sched("inside spring's gap", newYork)}, utc(3, 6, 0, 0), true, utc(3, 15, 6, 0)},
		{"spring: a range the clock skips, asked as the week before's ends", UserNotificationPreference{Schedule: sched("inside spring's gap", newYork)}, utc(3, 1, 7, 30), true, utc(3, 15, 6, 0)},
		// 01:30-01:45 shows twice on 1 November: 05:30 UTC (EDT) and 06:30 UTC (EST).
		{"autumn: before the hour", UserNotificationPreference{Schedule: sched("in autumn's repeated hour", newYork)}, utc(11, 1, 4, 0), true, utc(11, 1, 5, 30)},
		{"autumn: between the hour's two showings", UserNotificationPreference{Schedule: sched("in autumn's repeated hour", newYork)}, utc(11, 1, 5, 50), true, utc(11, 1, 6, 30)},
		{"autumn: the hour again", UserNotificationPreference{Schedule: sched("in autumn's repeated hour", newYork)}, utc(11, 1, 6, 35), false, time.Time{}},
		{"autumn: after both", UserNotificationPreference{Schedule: sched("in autumn's repeated hour", newYork)}, utc(11, 1, 6, 45), true, utc(11, 8, 6, 30)},

		// A schedule decides by itself; quiet_hours_enabled is the daily window's switch only.
		{"a schedule overrides the daily window", UserNotificationPreference{Schedule: sched("weekdays only", kolkata),
			QuietHoursEnabled: true, QuietHoursStart: str("09:00"), QuietHoursEnd: str("17:00"), QuietHoursTZ: str(kolkata)}, in(ist, 11, 4, 10, 0, 0), false, time.Time{}},
		{"a schedule needs no switch", UserNotificationPreference{Schedule: sched("weekdays only", kolkata)}, in(ist, 11, 7, 12, 0, 0), true, in(ist, 11, 9, 9, 0, 0)},
		// No schedule: the daily window, as it was.
		{"no schedule, no window", UserNotificationPreference{}, utc(11, 4, 23, 0), false, time.Time{}},
		{"no schedule, the daily window", UserNotificationPreference{
			QuietHoursEnabled: true, QuietHoursStart: str("22:00"), QuietHoursEnd: str("07:00"), QuietHoursTZ: str("UTC")}, utc(11, 4, 23, 0), true, utc(11, 5, 7, 0)},
	}
	for _, c := range cases {
		got, held := c.pref.QuietHoursUntil(c.now)
		want := c.until
		if !c.held {
			want = c.now
		}
		if held != c.held || !got.Equal(want) {
			t.Errorf("%s: held %v until %v, want held %v until %v", c.name, held, got.UTC(), c.held, want.UTC())
		}
	}
}

// Over a grid of instants, every five minutes across both of New York's
// changes and in Kolkata, for every week in each zone: a person held is held
// until exactly the moment the clock reaches a range (held a second before
// it, not held at it), and no instant of the grid between is free; a person
// not held is told now.
func TestQuietHoursUntilIsExactOverAGrid(t *testing.T) {
	stretches := [][2]time.Time{
		{time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
	}
	for name, w := range scheduleWeeks {
		for _, tz := range []string{"Asia/Kolkata", "America/New_York"} {
			p := &UserNotificationPreference{Schedule: &Schedule{TZ: tz, Week: w}}
			for _, s := range stretches {
				var grid []time.Time
				for at := s[0]; at.Before(s[1]); at = at.Add(5 * time.Minute) {
					grid = append(grid, at)
				}
				// nextFree[i] is the first instant of the grid after i that isn't held.
				nextFree := make([]time.Time, len(grid))
				var free time.Time
				for i := len(grid) - 1; i >= 0; i-- {
					nextFree[i] = free
					if _, held := p.QuietHoursUntil(grid[i]); !held {
						free = grid[i]
					}
				}
				for i, at := range grid {
					until, held := p.QuietHoursUntil(at)
					if !held {
						if !until.Equal(at) {
							t.Errorf("%s in %s at %v: not held, but told %v", name, tz, at, until)
						}
						continue
					}
					if !until.After(at) {
						t.Errorf("%s in %s at %v: held until %v, which isn't later", name, tz, at, until)
						continue
					}
					if _, still := p.QuietHoursUntil(until); still {
						t.Errorf("%s in %s at %v: held until %v, but held then too", name, tz, at, until)
					}
					if _, before := p.QuietHoursUntil(until.Add(-time.Second)); !before {
						t.Errorf("%s in %s at %v: held until %v, but free a second before", name, tz, at, until)
					}
					if f := nextFree[i]; !f.IsZero() && f.Before(until) {
						t.Errorf("%s in %s at %v: held until %v, but free at %v", name, tz, at, until, f)
					}
				}
			}
		}
	}
}
