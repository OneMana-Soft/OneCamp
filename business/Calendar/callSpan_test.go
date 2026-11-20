package Calendar

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A series' instances tile time: each call is exactly one instance's, the
// one whose start (less callLead) it last followed, or the first's when it
// came before them all (V-1061).
func TestASeriesCallIsTheInstanceItStartedIn(t *testing.T) {
	owners := func(starts []time.Time, at *time.Time) []int {
		var out []int
		for k, s := range starts {
			var next *time.Time
			if k+1 < len(starts) {
				next = &starts[k+1]
			}
			span := instanceCallSpan(s, k == 0, next)
			if (CallKeys{Span: &span}).Holds(at, nil) {
				out = append(out, k+1)
			}
		}
		return out
	}
	ptr := func(t time.Time) *time.Time { return &t }
	daily := []time.Time{}
	weekly := []time.Time{}
	first := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for k := 0; k < 3; k++ {
		daily = append(daily, first.AddDate(0, 0, k))
		weekly = append(weekly, first.AddDate(0, 0, 7*k))
	}
	for _, c := range []struct {
		name   string
		starts []time.Time
		at     time.Time
		want   int
	}{
		{"opened five minutes before the second", daily, daily[1].Add(-5 * time.Minute), 2},
		{"opened callLead before the second", daily, daily[1].Add(-callLead), 2},
		{"opened just after callLead before the third ran out", daily, daily[2].Add(-callLead - time.Second), 2},
		{"opened callLead before the third", daily, daily[2].Add(-callLead), 3},
		{"ran on past the second's end", daily, daily[1].Add(20 * time.Hour), 2},
		{"on a day with no instance", weekly, weekly[0].AddDate(0, 0, 2), 1},
		{"before the first", weekly, weekly[0].AddDate(0, 0, -3), 1},
		{"after the last", daily, daily[2].AddDate(0, 0, 5), 3},
	} {
		if got := owners(c.starts, ptr(c.at)); !slices.Equal(got, []int{c.want}) {
			t.Errorf("a call %s (%s) is instance %v's, want %d's alone", c.name, c.at.Format(time.RFC3339), got, c.want)
		}
	}
	if got := owners(daily, nil); len(got) != 0 {
		t.Errorf("a call with neither a start nor an end is instance %v's, want the series' alone", got)
	}
	// A call with no start is placed by its end.
	ended := daily[1].Add(10 * time.Minute)
	second, firstSpan := instanceCallSpan(daily[1], false, &daily[2]), instanceCallSpan(daily[0], true, &daily[1])
	if !(CallKeys{Span: &second}).Holds(nil, &ended) || (CallKeys{Span: &firstSpan}).Holds(nil, &ended) {
		t.Error("a call with no start that ended in the second isn't the second's alone")
	}
	if (CallKeys{Span: &second}).Holds(ptr(daily[0]), &ended) {
		t.Error("a call that started in the first and ended in the second is the second's: its start places it")
	}
	if !(CallKeys{}).Holds(nil, nil) || !(CallKeys{}).Holds(ptr(first.AddDate(-1, 0, 0)), nil) {
		t.Error("a single event or a series' own id doesn't hold every call it filed")
	}
}

// A series split by "this and following" keeps one call room (cloneEvent
// keeps CallRoom), so its halves tile time together: neither end of the split
// holds the other half's calls (CR-338 probe 1).
func TestASplitSeriesHalfHoldsNoneOfTheOtherHalfsCalls(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	s := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	h.store.events[uuid.MustParse(s.ID)].CallRoom = "room-split"
	in := h.between(sam, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 12, 0, 0, 0, 0, lon))
	later := "Stand-up (later)"
	if _, err := h.s.Update(ctx, sam, in[2].ID, EventPatch{Sequence: &in[2].Sequence, Title: &later, Scope: ScopeFollowing}); err != nil {
		t.Fatal(err)
	}
	in = h.between(sam, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 12, 0, 0, 0, 0, lon))
	var oldLast, newFirst *Event
	for k := range in {
		if strings.Contains(in[k].Title, "later") {
			if newFirst == nil {
				newFirst = &in[k]
			}
		} else {
			oldLast = &in[k]
		}
	}
	if oldLast == nil || newFirst == nil {
		t.Fatalf("instances after the split: %d", len(in))
	}
	ko, err := h.s.CallArtefactKeys(ctx, sam, oldLast.ID)
	if err != nil {
		t.Fatal(err)
	}
	kn, err := h.s.CallArtefactKeys(ctx, sam, newFirst.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("old last %s span %+v room %q; new first %s span %+v room %q", oldLast.ID, *ko.Span, ko.Room, newFirst.ID, *kn.Span, kn.Room)
	firstCall := time.Date(2026, 10, 20, 9, 16, 0, 0, lon)  // held in the old series' first instance
	fourthCall := time.Date(2026, 11, 10, 9, 14, 0, 0, lon) // held in the new series' second instance
	if ko.Room == kn.Room && ko.Holds(&fourthCall, nil) {
		t.Errorf("the old series' last instance holds the new series' second instance's call (shared room %q)", ko.Room)
	}
	if ko.Room == kn.Room && kn.Holds(&firstCall, nil) {
		t.Errorf("the new series' first instance holds the old series' first instance's call (shared room %q)", kn.Room)
	}
}

// A first instance moved before the series' start is still the only one open
// before it: the next one finds it (CR-338 probe 2).
func TestOnlyTheEarliestInstanceHoldsACallBeforeThemAll(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	h.series(sam, "Stand-up", "FREQ=DAILY;COUNT=3", time.Date(2026, 10, 20, 9, 0, 0, 0, lon), 30)
	in := h.between(sam, time.Date(2026, 10, 17, 0, 0, 0, 0, lon), time.Date(2026, 10, 24, 0, 0, 0, 0, lon))
	st, en := timed(time.Date(2026, 10, 19, 9, 0, 0, 0, lon), 30, "Europe/London")
	if _, err := h.s.Update(ctx, sam, in[0].ID, EventPatch{Sequence: &in[0].Sequence, Start: &st, End: &en, Scope: ScopeThis}); err != nil {
		t.Fatalf("moving the first a day earlier: %v", err)
	}
	in = h.between(sam, time.Date(2026, 10, 17, 0, 0, 0, 0, lon), time.Date(2026, 10, 24, 0, 0, 0, 0, lon))
	early := time.Date(2026, 10, 18, 12, 0, 0, 0, lon)
	var holders []string
	for _, e := range in {
		k, err := h.s.CallArtefactKeys(ctx, sam, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s span %+v", e.ID, *k.Span)
		if k.Holds(&early, nil) {
			holders = append(holders, e.ID)
		}
	}
	if len(holders) != 1 {
		t.Errorf("a call before them all is held by %d instances %v, want 1", len(holders), holders)
	}
}

// callSpanOf places an instance's neighbours as every view does: past a
// skipped date or a cancelled instance, by a moved instance's real start
// (past the next, or before the first), across both halves of a split
// series, and on either side of a change of clocks (V-1061).
func TestAnInstanceSpanEndsAtItsLiveNeighbours(t *testing.T) {
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	z := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, time.UTC) }
	var open time.Time
	// A daily Stand-up of five at 09:00 London, 22 to 26 October. The clocks
	// go back on the 25th, so the 4th and 5th start an hour later in UTC.
	moved := func(t *testing.T, h *harness, sam *model.UserInfo, e Event, at time.Time) {
		st, en := timed(at, 30, "Europe/London")
		if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: &e.Sequence, Start: &st, End: &en, Scope: ScopeThis}); err != nil {
			t.Fatalf("moving %s: %v", e.ID, err)
		}
	}
	for _, c := range []struct {
		name string
		edit func(t *testing.T, h *harness, sam *model.UserInfo, s *Event, in []Event) map[string]CallSpan
	}{
		{"a change of clocks", func(t *testing.T, h *harness, sam *model.UserInfo, s *Event, in []Event) map[string]CallSpan {
			return map[string]CallSpan{in[0].ID: {open, z(23, 7, 45)}, in[2].ID: {z(24, 7, 45), z(25, 8, 45)},
				in[3].ID: {z(25, 8, 45), z(26, 8, 45)}, in[4].ID: {z(26, 8, 45), open}}
		}},
		{"a skipped date", func(t *testing.T, h *harness, sam *model.UserInfo, s *Event, in []Event) map[string]CallSpan {
			h.store.events[uuid.MustParse(s.ID)].Exdates = []time.Time{z(23, 8, 0)}
			return map[string]CallSpan{in[0].ID: {open, z(24, 7, 45)}, in[2].ID: {z(24, 7, 45), z(25, 8, 45)}}
		}},
		{"a cancelled instance", func(t *testing.T, h *harness, sam *model.UserInfo, s *Event, in []Event) map[string]CallSpan {
			title := "Stand-up (off)"
			if _, err := h.s.Update(ctx, sam, in[1].ID, EventPatch{Sequence: &in[1].Sequence, Title: &title, Scope: ScopeThis}); err != nil {
				t.Fatal(err)
			}
			for _, e := range h.store.events {
				if e.SeriesID != nil && e.SeriesID.String() == s.ID {
					e.Status = statusCancelled
				}
			}
			return map[string]CallSpan{in[0].ID: {open, z(24, 7, 45)}, in[2].ID: {z(24, 7, 45), z(25, 8, 45)}}
		}},
		{"an instance moved past the next", func(t *testing.T, h *harness, sam *model.UserInfo, s *Event, in []Event) map[string]CallSpan {
			moved(t, h, sam, in[1], time.Date(2026, 10, 24, 12, 0, 0, 0, lon))
			return map[string]CallSpan{in[0].ID: {open, z(24, 7, 45)}, in[2].ID: {z(24, 7, 45), z(24, 10, 45)},
				in[1].ID: {z(24, 10, 45), z(25, 8, 45)}}
		}},
		{"the first moved before the series' start", func(t *testing.T, h *harness, sam *model.UserInfo, s *Event, in []Event) map[string]CallSpan {
			moved(t, h, sam, in[0], time.Date(2026, 10, 21, 9, 0, 0, 0, lon))
			return map[string]CallSpan{in[0].ID: {open, z(23, 7, 45)}, in[1].ID: {z(23, 7, 45), z(24, 7, 45)}}
		}},
		{"a split from the third", func(t *testing.T, h *harness, sam *model.UserInfo, s *Event, in []Event) map[string]CallSpan {
			h.store.events[uuid.MustParse(s.ID)].CallRoom = "room-split"
			later := "Stand-up (later)"
			if _, err := h.s.Update(ctx, sam, in[2].ID, EventPatch{Sequence: &in[2].Sequence, Title: &later, Scope: ScopeFollowing}); err != nil {
				t.Fatal(err)
			}
			var halves []string
			for _, e := range h.between(sam, z(21, 0, 0), z(28, 0, 0)) {
				if strings.Contains(e.Title, "later") {
					halves = append(halves, e.ID)
				}
			}
			if len(halves) != 3 {
				t.Fatalf("the later half has %d instances, want 3", len(halves))
			}
			return map[string]CallSpan{in[0].ID: {open, z(23, 7, 45)}, in[1].ID: {z(23, 7, 45), z(24, 7, 45)},
				halves[0]: {z(24, 7, 45), z(25, 8, 45)}, halves[2]: {z(26, 8, 45), open}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			sam := h.member("Sam")
			s := h.series(sam, "Stand-up", "FREQ=DAILY;COUNT=5", time.Date(2026, 10, 22, 9, 0, 0, 0, lon), 30)
			in := h.between(sam, z(21, 0, 0), z(28, 0, 0))
			if len(in) != 5 {
				t.Fatalf("%d instances, want 5", len(in))
			}
			for id, want := range c.edit(t, h, sam, s, in) {
				k, err := h.s.CallArtefactKeys(ctx, sam, id)
				if err != nil {
					t.Fatal(err)
				}
				if k.Span == nil || !k.Span.From.Equal(want.From) || !k.Span.To.Equal(want.To) {
					t.Errorf("%s's span is %+v, want %+v", id, k.Span, want)
				}
			}
		})
	}
}
