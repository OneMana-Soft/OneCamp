package Calendar

import (
	"context"
	"strings"
	"testing"
	"time"

	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// readCount counts the store reads a page of search entries makes.
type readCount struct {
	*memStore
	many, overrides int
}

func (r *readCount) getMany(ctx context.Context, ids []uuid.UUID) ([]*models.Event, error) {
	r.many++
	return r.memStore.getMany(ctx, ids)
}

func (r *readCount) overridesOf(ctx context.Context, series []uuid.UUID, w models.Window) ([]*models.Event, error) {
	r.overrides++
	return r.memStore.overridesOf(ctx, series, w)
}

// Search shows a repeating event by when it is next on, never by its first
// instance (V-22): the first of its instances that hasn't ended, by its own
// rule and exceptions, a deleted instance skipped and a moved one where it
// went. An event that doesn't repeat, or whose instances are all over, keeps
// its entry. A page's series and their overrides are read once each.
func TestSearchShowsARepeatingEventByWhenItIsNextOn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	prev, prevMoves := stored, moves
	stored = h.s
	// The calendar is read from its store: the move is done.
	moves = &mover{isDone: func() (bool, error) { return true, nil }}
	t.Cleanup(func() { stored, moves = prev, prevMoves })
	sam := h.member("Sam")
	standup := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	planning := h.series(sam, "Planning", "FREQ=DAILY", time.Date(2026, 10, 26, 9, 0, 0, 0, lon), 60)
	kickoff := h.series(sam, "Kick-off", "FREQ=WEEKLY;COUNT=2", time.Date(2026, 10, 6, 11, 0, 0, 0, lon), 60)
	gym, err := h.s.Create(ctx, sam, EventInput{Title: "Gym", Start: When{Date: "2026-10-21"}, End: When{Date: "2026-10-22"}, Recurrence: &Recurrence{Rule: "FREQ=WEEKLY"}})
	if err != nil {
		t.Fatal(err)
	}
	st, en := timed(time.Date(2026, 11, 2, 15, 0, 0, 0, lon), 30, "Europe/London")
	review, err := h.s.Create(ctx, sam, EventInput{Title: "Review", Start: st, End: en})
	if err != nil {
		t.Fatal(err)
	}

	// Each entry as the index writes it: a series says its first instance.
	entryOf := func(e *Event) *openSearchStruct.OpenSearchEvent {
		t.Helper()
		rows, err := h.store.getMany(ctx, []uuid.UUID{uuid.MustParse(e.ID)})
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s's row: %v", e.Title, err)
		}
		entry := searchEntryOf(rows[0], nil, models.Calendar{}, nil)
		return &entry
	}
	when := func(e *openSearchStruct.OpenSearchEvent) string {
		if e.EventStartDate != "" {
			return e.EventStartDate + " to " + e.EventEndDate
		}
		return time.Unix(e.EventStart, 0).In(lon).Format("Mon 2 Jan 15:04") + "-" + time.Unix(e.EventEnd, 0).In(lon).Format("15:04")
	}
	shown := func(es ...*Event) string {
		t.Helper()
		var entries []*openSearchStruct.OpenSearchEvent
		for _, e := range es {
			entries = append(entries, entryOf(e))
		}
		ShowNextInstances(ctx, entries)
		var out []string
		for i, e := range entries {
			out = append(out, es[i].Title+" "+when(e))
		}
		return strings.Join(out, ", ")
	}
	instanceOn := func(title string, day time.Time) Event {
		t.Helper()
		for _, e := range h.between(sam, day, day.AddDate(0, 0, 1)) {
			if e.Title == title {
				return e
			}
		}
		t.Fatalf("no %s on %s", title, day.Format("2 Jan"))
		return Event{}
	}

	// Wednesday 28 Oct, 09:30 in London: the stand-up is next on Tuesday,
	// today's planning is on now, today is a gym day, the kick-off is over.
	h.clock = time.Date(2026, 10, 28, 9, 30, 0, 0, lon)
	reads := &readCount{memStore: h.store}
	h.s.store = reads
	if got, want := shown(standup, planning, gym, kickoff, review), "Stand-up Tue 3 Nov 09:15-09:30, Planning Wed 28 Oct 09:00-10:00, Gym 2026-10-28 to 2026-10-29, Kick-off Tue 6 Oct 11:00-12:00, Review Mon 2 Nov 15:00-15:30"; got != want {
		t.Errorf("shown %s\n want %s", got, want)
	}
	if reads.many != 1 || reads.overrides != 1 {
		t.Errorf("read the events %d times and their overrides %d times; want once each", reads.many, reads.overrides)
	}
	h.s.store = h.store

	// The 3 Nov stand-up deleted: the next is 10 Nov.
	three := instanceOn("Stand-up", time.Date(2026, 11, 3, 0, 0, 0, 0, lon))
	if err := h.s.Delete(ctx, sam, three.ID, ScopeThis, seq(three.Sequence)); err != nil {
		t.Fatal(err)
	}
	if got, want := shown(standup), "Stand-up Tue 10 Nov 09:15-09:30"; got != want {
		t.Errorf("one deleted: shown %s, want %s", got, want)
	}

	// The 10 Nov one moved to Wednesday afternoon: it is where it went.
	ten := instanceOn("Stand-up", time.Date(2026, 11, 10, 0, 0, 0, 0, lon))
	p := patchAt(ten, ScopeThis)
	p.Start, p.End = whenAt(time.Date(2026, 11, 11, 14, 0, 0, 0, lon)), whenAt(time.Date(2026, 11, 11, 14, 15, 0, 0, lon))
	if _, err := h.s.Update(ctx, sam, ten.ID, p); err != nil {
		t.Fatal(err)
	}
	if got, want := shown(standup), "Stand-up Wed 11 Nov 14:00-14:15"; got != want {
		t.Errorf("one moved later: shown %s, want %s", got, want)
	}

	// The 17 Nov one moved before it, to Monday 9 Nov: that one is next.
	seventeen := instanceOn("Stand-up", time.Date(2026, 11, 17, 0, 0, 0, 0, lon))
	p = patchAt(seventeen, ScopeThis)
	p.Start, p.End = whenAt(time.Date(2026, 11, 9, 8, 0, 0, 0, lon)), whenAt(time.Date(2026, 11, 9, 8, 15, 0, 0, lon))
	if _, err := h.s.Update(ctx, sam, seventeen.ID, p); err != nil {
		t.Fatal(err)
	}
	if got, want := shown(standup), "Stand-up Mon 9 Nov 08:00-08:15"; got != want {
		t.Errorf("a later one moved first: shown %s, want %s", got, want)
	}

	// While the calendar isn't read from its store, entries are as written.
	t.Setenv("CALENDAR_STORE", "graph")
	if got, want := shown(standup), "Stand-up Tue 20 Oct 09:15-09:30"; got != want {
		t.Errorf("under the graph store: shown %s, want %s", got, want)
	}
}
