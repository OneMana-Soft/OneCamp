package business

import (
	"fmt"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/tests/timing"
)

type iv = calendarBusiness.Interval

func week(tz string, spans ...calendarBusiness.Span) *calendarBusiness.WorkingHours {
	w := map[string][]calendarBusiness.Span{}
	for d := 1; d <= 5; d++ {
		w[fmt.Sprint(d)] = spans
	}
	return &calendarBusiness.WorkingHours{TZ: tz, Week: w}
}

// The acceptance case: Jonas (London, 09:00–17:00) and Maya (India,
// 10:00–18:30) need 30 minutes; what is offered falls in both their hours.
func TestEveryoneIsOfferedTimesInTheirOwnHours(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, london) // Monday
	jonas := Person{ID: "jonas", Name: "Jonas", Hours: week("Europe/London", calendarBusiness.Span{Start: "09:00", End: "17:00"})}
	maya := Person{ID: "maya", Name: "Maya", Hours: week("Asia/Kolkata", calendarBusiness.Span{Start: "10:00", End: "18:30"})}
	got := Rank([]Person{jonas, maya}, Ask{Duration: 30 * time.Minute, From: from, To: from.Add(24 * time.Hour), Zone: london, Limit: 50})
	if len(got) == 0 {
		t.Fatal("nothing offered")
	}
	together := 0
	for i, s := range got {
		if s.Reason != "Everyone's free" {
			// The times most can meet come only after every time all can.
			if i < together {
				t.Fatalf("%v: %q before a time everyone can", s.Start, s.Reason)
			}
			continue
		}
		if i != together {
			t.Fatalf("a time everyone can comes after one they can't: %v", s.Start)
		}
		together++
		// India 18:30 is London 14:00 (BST): every offer ends by then, and starts at 09:00 London or later.
		if l := s.Start.In(london); l.Hour() < 9 || s.End.In(london).After(time.Date(2026, 10, 19, 14, 0, 0, 0, london)) {
			t.Errorf("outside someone's hours: %v–%v", l, s.End.In(london))
		}
	}
	// 09:00 to 14:00 London on a Monday: ten half hours.
	if together != 10 {
		t.Errorf("%d times everyone can, want 10", together)
	}
}

// A day of several ranges keeps its gap free of offers.
func TestALunchGapIsntOffered(t *testing.T) {
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	me := Person{ID: "me", Name: "You", Hours: week("UTC", calendarBusiness.Span{Start: "09:00", End: "12:30"}, calendarBusiness.Span{Start: "13:30", End: "17:00"})}
	for _, s := range Rank([]Person{me}, Ask{Duration: time.Hour, From: from, To: from.Add(24 * time.Hour), Limit: 50}) {
		if s.Start.Before(from.Add(13*time.Hour+30*time.Minute)) && s.End.After(from.Add(12*time.Hour+30*time.Minute)) {
			t.Errorf("offered across lunch: %v–%v", s.Start, s.End)
		}
		if s.Reason != "You're free" {
			t.Errorf("reason %q", s.Reason)
		}
	}
}

// With too few times for everyone, the times most can meet follow, each
// saying who can't and why.
func TestTooFewTimesForEveryoneFillWithMostFreeSayingWhy(t *testing.T) {
	from := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	to := from.Add(2 * time.Hour)
	me := Person{ID: "me", Name: "You"}
	jonas := Person{ID: "jonas", Name: "Jonas", Busy: []iv{{Start: from, End: to}}}
	maya := Person{ID: "maya", Name: "Maya", Hours: week("UTC", calendarBusiness.Span{Start: "10:00", End: "18:00"})}
	got := Rank([]Person{me, jonas, maya}, Ask{Duration: time.Hour, Step: time.Hour, From: from, To: to, Limit: 4})
	if len(got) != 2 {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	if got[0].Reason != "2 of 3 free · Jonas busy" || got[1].Reason != "1 of 3 free · Jonas busy · outside Maya's hours" {
		t.Errorf("reasons: %q, %q", got[0].Reason, got[1].Reason)
	}
}

// The asker's own busy time is never offered.
func TestTheAskersBusyTimeIsNeverOffered(t *testing.T) {
	from := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	me := Person{ID: "me", Name: "You", Busy: []iv{{Start: from, End: from.Add(time.Hour)}}}
	got := Rank([]Person{me, {ID: "jonas", Name: "Jonas"}}, Ask{Duration: time.Hour, Step: time.Hour, From: from, To: from.Add(2 * time.Hour)})
	if len(got) != 1 || !got[0].Start.Equal(from.Add(time.Hour)) {
		t.Errorf("%+v", got)
	}
}

// The engine's speed target (A3b proves it with real reads, A3a holds the
// ranking to it from the start): twenty people, each with their own hours in
// one of six zones and four meetings a working day, over fourteen days, are
// ranked well inside 150 ms of CPU; scaled under the race detector.
func TestTwentyPeopleOverFourteenDaysRankInBudget(t *testing.T) {
	zones := []string{"Europe/London", "Asia/Kolkata", "America/New_York", "Asia/Tokyo", "Australia/Sydney", "America/Los_Angeles"}
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	to := from.Add(14 * 24 * time.Hour)
	people := make([]Person, 20)
	for k := range people {
		var busy []iv
		for d := 0; d < 14; d++ {
			for m := 0; m < 4; m++ {
				s := from.Add(time.Duration(d)*24*time.Hour + time.Duration(8+2*m+k%3)*time.Hour)
				busy = append(busy, iv{Start: s, End: s.Add(45 * time.Minute)})
			}
		}
		people[k] = Person{ID: fmt.Sprint(k), Name: fmt.Sprint("P", k), Hours: week(zones[k%len(zones)], calendarBusiness.Span{Start: "08:00", End: "18:00"}), Busy: busy}
	}
	budget := 150 * time.Millisecond
	if timing.Race {
		budget *= 4
	}
	took := timing.FastestCPU(5, func() {
		Rank(people, Ask{Duration: 30 * time.Minute, Step: 15 * time.Minute, From: from, To: to, PerDay: 3, Limit: 12})
	})
	if took > budget {
		t.Errorf("ranking 20 people over 14 days took %v of CPU, over %v", took, budget)
	}
}
