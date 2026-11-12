package ICS

import (
	"fmt"
	"testing"
	"time"
)

// The VTIMEZONE walk (V-719): a zone's changes come out exactly as its
// rules make them, across a leap year's end where Go's zone data ends a
// year early, and the walk to 9999 is quick.

// zoneRule is a zone's yearly pair of changes, written from its rules
// rather than from any zone file, so the walk is checked against them.
type zoneRule struct {
	std, dst         int    // offsets, seconds east of UTC
	stdName, dstName string // abbreviations
	toDST, toStd     func(year int) time.Time
}

// sunday is the n-th Sunday of a month (n < 0 counts from its end) at hh:00 UTC.
func sunday(year int, month time.Month, n, hh int) time.Time {
	if n < 0 {
		last := time.Date(year, month+1, 0, hh, 0, 0, 0, time.UTC)
		return last.AddDate(0, 0, -int(last.Weekday()))
	}
	first := time.Date(year, month, 1, hh, 0, 0, 0, time.UTC)
	return first.AddDate(0, 0, (7-int(first.Weekday()))%7+7*(n-1))
}

var (
	// The EU's: last Sunday of March and of October, 01:00 UTC.
	kyivRule = zoneRule{std: 2 * 3600, dst: 3 * 3600, stdName: "EET", dstName: "EEST",
		toDST: func(y int) time.Time { return sunday(y, time.March, -1, 1) },
		toStd: func(y int) time.Time { return sunday(y, time.October, -1, 1) }}
	// The US's since 2007: second Sunday of March at 02:00 EST, first of
	// November at 02:00 EDT.
	newYorkRule = zoneRule{std: -5 * 3600, dst: -4 * 3600, stdName: "EST", dstName: "EDT",
		toDST: func(y int) time.Time { return sunday(y, time.March, 2, 7) },
		toStd: func(y int) time.Time { return sunday(y, time.November, 1, 6) }}
)

// changes are the rule's changes in [start, end].
func (r zoneRule) changes(start, end time.Time) []transition {
	var out []transition
	for y := start.Year(); y <= end.Year(); y++ {
		for _, c := range []transition{
			{at: r.toDST(y), from: r.std, to: r.dst, dst: true, name: r.dstName},
			{at: r.toStd(y), from: r.dst, to: r.std, name: r.stdName},
		} {
			if !c.at.Before(start) && !c.at.After(end) {
				out = append(out, c)
			}
		}
	}
	return out
}

// show is a change as the failures say it.
func show(c transition) string {
	return fmt.Sprintf("%s %d->%d dst=%v %s", c.at.UTC().Format(time.RFC3339), c.from, c.to, c.dst, c.name)
}

func sameTransitions(t *testing.T, zone string, got, want []transition) {
	t.Helper()
	for i := 0; i < len(got) || i < len(want); i++ {
		switch {
		case i >= len(got):
			t.Fatalf("%s: no change %d, want %s", zone, i, show(want[i]))
		case i >= len(want):
			t.Fatalf("%s: change %d %s is one the rules don't make", zone, i, show(got[i]))
		case !got[i].at.Equal(want[i].at) || got[i].from != want[i].from || got[i].to != want[i].to ||
			got[i].dst != want[i].dst || got[i].name != want[i].name:
			t.Fatalf("%s: change %d is %s, want %s", zone, i, show(got[i]), show(want[i]))
		}
	}
}

func TestAZonesChangesAreExactlyItsRulesAcrossALeapYearsEnd(t *testing.T) {
	for _, c := range []struct {
		zone string
		rule zoneRule
	}{{"Europe/Kyiv", kyivRule}, {"America/New_York", newYorkRule}} {
		loc := mustZone(t, c.zone)
		// 2028 and 2040 are leap years; past 2037 every zone file leaves the
		// years to its rule, so the second span holds on any host's tzdata.
		for _, y := range []int{2028, 2040} {
			start, end := time.Date(y, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(y+2, 7, 1, 0, 0, 0, 0, time.UTC)
			want := c.rule.changes(start, end)
			if len(want) != 4 {
				t.Fatalf("%s from mid-%d: the rule gives %d changes, want 4", c.zone, y, len(want))
			}
			sameTransitions(t, c.zone, transitions(loc, start, end), want)
		}
	}
}

func TestAnEventIn9999HasItsZoneWalkedQuickly(t *testing.T) {
	for _, c := range []struct {
		zone string
		rule zoneRule
	}{{"Europe/Kyiv", kyivRule}, {"America/New_York", newYorkRule}} {
		loc := mustZone(t, c.zone)
		start, end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
		done := make(chan []transition, 1)
		go func() { done <- transitions(loc, start, end) }()
		select {
		case got := <-done:
			want := c.rule.changes(start, end)
			if n := len(got); n == 0 || got[n-1].at.Year() != 9999 {
				t.Fatalf("%s: the walk to 9999 stopped early (%d changes)", c.zone, n)
			}
			sameTransitions(t, c.zone, got, want)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the walk from 2026 to 9999 took over 5 s", c.zone)
		}
	}
}
