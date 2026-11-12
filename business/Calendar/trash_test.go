package Calendar

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The trash (gcal-1a head B) holds what its person deleted in the last 30
// days, newest first, each with when and by whom: an event deleted 31 days
// ago is no longer listed (and stays deleted: nothing is purged), one
// deleted 29 days ago is. Nobody's trash lists another's events.
func TestTheTrashHoldsTheLast30Days(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	now := h.clock
	h.clock = now.AddDate(0, 0, -40)
	old, recent, newest := h.create(sam, "Old offsite"), h.create(sam, "Board prep"), h.create(sam, "Lunch")
	mine := h.create(maya, "Maya's own")
	for _, d := range []struct {
		id  string
		ago time.Duration
	}{{old.ID, 31 * 24 * time.Hour}, {recent.ID, 29 * 24 * time.Hour}, {newest.ID, time.Hour}} {
		h.clock = now.Add(-d.ago)
		if err := h.s.Delete(ctx, sam, d.id, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	h.clock = now.Add(-2 * time.Hour)
	if err := h.s.Delete(ctx, maya, mine.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	h.clock = now

	trash, err := h.s.Trash(ctx, sam, nil)
	if err != nil || len(trash) != 2 || trash[0].ID != newest.ID || trash[1].ID != recent.ID {
		t.Fatalf("Sam's trash: %+v %v", trash, err)
	}
	for _, e := range trash {
		if e.DeletedAt == nil || e.DeletedBy == nil || e.DeletedBy.UUID != sam.UserDgraphInfo.Uuid || e.DeletedBy.Name != "Sam" {
			t.Errorf("%s: deleted %v by %+v", e.Title, e.DeletedAt, e.DeletedBy)
		}
	}
	// Paged from the last it showed, still within the 30 days.
	if page, err := h.s.Trash(ctx, sam, &TrashAfter{DeletedAt: *trash[0].DeletedAt, ID: trash[0].ID}); err != nil || len(page) != 1 || page[0].ID != recent.ID {
		t.Errorf("the next page: %+v %v", page, err)
	}
	if page, err := h.s.Trash(ctx, sam, &TrashAfter{DeletedAt: *trash[1].DeletedAt, ID: trash[1].ID}); err != nil || len(page) != 0 {
		t.Errorf("past the 30 days: %+v %v", page, err)
	}
	// The old one is still in the store, deleted, and comes back by its id.
	if got, err := h.s.Restore(ctx, sam, old.ID); err != nil || got.DeletedAt != nil {
		t.Errorf("the 31-day-old one restored: %+v %v", got, err)
	}
	if theirs, err := h.s.Trash(ctx, maya, nil); err != nil || len(theirs) != 1 || theirs[0].ID != mine.ID {
		t.Errorf("Maya's trash: %+v %v", theirs, err)
	}
}

// calendar V-603: what is deleted at once (a series with its exceptions, a
// batch) shares one deletion time, so a page of the trash can end inside such
// a group. The next page goes on from the last event the page gave, by its
// time and its id, an instance's too, and holds the rest of the group: every
// event shows once. An older client that sends the time alone gets what it
// always did, what was deleted before then.
func TestTheTrashPagesThroughEventsDeletedAtOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	var ids []string
	for i := 0; i < trashPage-1; i++ {
		ids = append(ids, h.create(sam, fmt.Sprintf("Event %d", i)).ID)
	}
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	in := h.between(sam, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon))
	if len(in) != 3 || in[0].SeriesID != e.ID {
		t.Fatalf("the series' instances: %+v", in)
	}
	// All at one moment: 49 events and two instances, 51 in the trash.
	h.clock = h.clock.Add(time.Hour)
	for _, id := range ids {
		if err := h.s.Delete(ctx, sam, id, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, inst := range in[1:] {
		if err := h.s.Delete(ctx, sam, inst.ID, ScopeThis, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, inst.ID)
	}

	seen := map[string]int{}
	var after *TrashAfter
	for pages := 0; ; pages++ {
		page, err := h.s.Trash(ctx, sam, after)
		if err != nil || pages > 2 {
			t.Fatalf("page %d: %d events, %v", pages+1, len(page), err)
		}
		for _, ev := range page {
			seen[ev.ID]++
		}
		if len(page) < trashPage {
			break
		}
		last := page[len(page)-1]
		after = &TrashAfter{DeletedAt: *last.DeletedAt, ID: last.ID}
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("%s shows %d times in the trash, want once", id, seen[id])
		}
	}
	if len(seen) != len(ids) {
		t.Errorf("the trash shows %d events, want %d", len(seen), len(ids))
	}

	// An older client sends the time alone: nothing was deleted before it.
	first, _ := h.s.Trash(ctx, sam, nil)
	last := first[len(first)-1]
	if page, err := h.s.Trash(ctx, sam, &TrashAfter{DeletedAt: *last.DeletedAt}); err != nil || len(page) != 0 {
		t.Errorf("from the time alone: %d events, %v; want none", len(page), err)
	}
	// An id that is no event's is refused in words.
	var input *EventInputError
	if _, err := h.s.Trash(ctx, sam, &TrashAfter{DeletedAt: *last.DeletedAt, ID: "not-an-id"}); !errors.As(err, &input) {
		t.Errorf("from a bad id: %v, want an input error", err)
	}
}

// trashSince is where the 30 days start: trashDays before now.
func TestTheTrashWindowIs30Days(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	if got := trashSince(now); !got.Equal(now.Add(-30 * 24 * time.Hour)) {
		t.Errorf("%v", got)
	}
}
