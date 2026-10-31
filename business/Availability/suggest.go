package business

// Ranked suggestions (calendar A3): the times everyone can meet, each in
// their own working hours and zone, first; then, when there are too few of
// those, the times most of them can, each saying in words who can't and why
// ("3 of 4 free · Jonas busy", "· outside Maya's hours"). Pure: the engine
// reads each person's hours and busy times, then asks Rank.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
)

// Person is someone a meeting is for: their working hours (nil when they
// keep none: any time is in hours) and when they are busy.
type Person struct {
	ID    string
	Name  string
	Hours *calendarBusiness.WorkingHours
	Busy  []calendarBusiness.Interval
}

// Suggestion is a time, who is free then, and why it is offered, in words.
type Suggestion struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Free   []string  `json:"free"`
	Away   []string  `json:"away,omitempty"`
	Reason string    `json:"reason"`
}

// Ask is what a ranking is for: how long, from when to when, starting on
// steps of Step after Earliest, at most PerDay a day (in Zone, the asker's)
// and Limit in all.
type Ask struct {
	Duration, Step time.Duration
	From, To       time.Time
	Earliest       time.Time
	Zone           *time.Location
	PerDay, Limit  int
}

// unavailable is when a person can't meet: busy, or outside their hours.
type unavailable struct {
	busy, outside []calendarBusiness.Interval
}

// outsideHours is the time between from and to that isn't in hours. Pure.
func outsideHours(h *calendarBusiness.WorkingHours, from, to time.Time) []calendarBusiness.Interval {
	if h == nil {
		return nil
	}
	var out []calendarBusiness.Interval
	at := from
	for _, w := range calendarBusiness.Windows(*h, from, to) {
		if w.Start.After(at) {
			out = append(out, calendarBusiness.Interval{Start: at, End: w.Start})
		}
		at = w.End
	}
	if to.After(at) {
		out = append(out, calendarBusiness.Interval{Start: at, End: to})
	}
	return out
}

// clashes is whether [s, e) meets any of in (sorted, merged), moving *i past
// what ends before s: the starts walked are rising, so no interval is looked
// at twice.
func clashes(in []calendarBusiness.Interval, i *int, s, e time.Time) bool {
	for *i < len(in) && !in[*i].End.After(s) {
		*i++
	}
	return *i < len(in) && in[*i].Start.Before(e)
}

// Rank is the suggestions for people (the asker first, who is always free
// and in hours in what is offered), everyone free first, earliest first;
// then, to fill Limit, the times most can meet, the fewest away first. Pure.
func Rank(people []Person, ask Ask) []Suggestion {
	if len(people) == 0 || ask.Duration <= 0 || !ask.To.After(ask.From) {
		return nil
	}
	step := ask.Step
	if step <= 0 {
		step = 30 * time.Minute
	}
	zone := ask.Zone
	if zone == nil {
		zone = time.UTC
	}
	limit := ask.Limit
	if limit <= 0 {
		limit = 12
	}
	un := make([]unavailable, len(people))
	for k, p := range people {
		un[k] = unavailable{busy: calendarBusiness.MergeBusy(p.Busy), outside: outsideHours(p.Hours, ask.From, ask.To)}
	}
	start := ask.From
	if ask.Earliest.After(start) {
		start = ask.Earliest
	}
	// Starts on the step, in the asker's zone: 10:00, 10:30, never 10:07.
	local := start.In(zone)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	first := day
	for first.Before(start) {
		first = first.Add(step)
	}

	type candidate struct {
		s       Suggestion
		awayN   int
		dayKey  string
		allFree bool
	}
	bi := make([]int, len(people))
	oi := make([]int, len(people))
	var all, partial []candidate
	perDay := map[string]int{}
	for s := first; !s.Add(ask.Duration).After(ask.To); s = s.Add(step) {
		e := s.Add(ask.Duration)
		c := candidate{s: Suggestion{Start: s.UTC(), End: e.UTC()}, dayKey: s.In(zone).Format("2006-01-02")}
		var busyNames, outsideNames []string
		for k := range people {
			busy := clashes(un[k].busy, &bi[k], s, e)
			outside := clashes(un[k].outside, &oi[k], s, e)
			switch {
			case !busy && !outside:
				c.s.Free = append(c.s.Free, people[k].ID)
			case busy:
				busyNames = append(busyNames, people[k].Name)
				c.s.Away = append(c.s.Away, people[k].ID)
			default:
				outsideNames = append(outsideNames, people[k].Name)
				c.s.Away = append(c.s.Away, people[k].ID)
			}
			// The asker's own time only.
			if k == 0 && (busy || outside) {
				break
			}
		}
		if len(c.s.Free) == 0 || c.s.Free[0] != people[0].ID {
			continue
		}
		c.awayN = len(c.s.Away)
		if c.awayN == 0 {
			c.allFree = true
			c.s.Reason = "Everyone's free"
			if len(people) == 1 {
				c.s.Reason = "You're free"
			}
			if ask.PerDay > 0 && perDay[c.dayKey] >= ask.PerDay {
				continue
			}
			perDay[c.dayKey]++
			all = append(all, c)
			if len(all) >= limit {
				break
			}
			continue
		}
		c.s.Reason = reason(len(people)-c.awayN, len(people), busyNames, outsideNames)
		partial = append(partial, c)
	}
	out := make([]Suggestion, 0, limit)
	for _, c := range all {
		out = append(out, c.s)
	}
	if len(out) < limit && len(people) > 1 {
		sort.SliceStable(partial, func(i, j int) bool { return partial[i].awayN < partial[j].awayN })
		shown := map[string]int{}
		for _, c := range partial {
			if len(out) >= limit {
				break
			}
			if ask.PerDay > 0 && perDay[c.dayKey]+shown[c.dayKey] >= ask.PerDay {
				continue
			}
			shown[c.dayKey]++
			out = append(out, c.s)
		}
	}
	return out
}

// reason says who can't make a time and why: "3 of 4 free · Jonas busy",
// "2 of 3 free · outside Maya's hours". Pure.
func reason(free, of int, busy, outside []string) string {
	parts := []string{fmt.Sprintf("%d of %d free", free, of)}
	if len(busy) > 0 {
		parts = append(parts, names(busy)+" busy")
	}
	if len(outside) > 0 {
		parts = append(parts, "outside "+possessive(names(outside))+" hours")
	}
	return strings.Join(parts, " · ")
}

// names joins names in words: "Jonas", "Jonas and Maya", "Jonas, Maya and 2 more".
func names(n []string) string {
	switch {
	case len(n) == 1:
		return n[0]
	case len(n) == 2:
		return n[0] + " and " + n[1]
	case len(n) == 3:
		return n[0] + ", " + n[1] + " and " + n[2]
	default:
		return fmt.Sprintf("%s, %s and %d more", n[0], n[1], len(n)-2)
	}
}

// possessive is "Maya's", "Jonas's", or for several people "Jonas and Maya's".
func possessive(s string) string {
	return s + "'s"
}
