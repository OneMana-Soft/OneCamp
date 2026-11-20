package Calendar

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/google/uuid"
)

// drainAtOnce runs every calendar.sync entry through the sink the moment it
// is added, as a drain that comes before anything after the enqueue (the
// refs a sweep keeps after its import), and answers the events asked for.
func drainAtOnce(t *testing.T, h *harness) *[]string {
	t.Helper()
	var synced []string
	prev := enqueueSearch
	enqueueSearch = func(ctx context.Context, es ...outboxBusiness.Entry) error {
		for _, e := range es {
			if e.Topic == syncTopic {
				synced = append(synced, e.Entities["event"])
				_ = h.s.runSyncSink(ctx, outboxBusiness.Event{ID: 1, EventID: uuid.NewString(), Topic: e.Topic, Kind: e.Kind, Key: e.Key, Entities: e.Entities, Attempt: 1})
			}
		}
		return nil
	}
	t.Cleanup(func() { enqueueSearch = prev })
	return &synced
}

// CR-383 F1: the sweep's own read brings a new Google event in with its
// search entries only. It is Google's already: it asks for no sync entry,
// and a drain run the moment it is read (before its ref is kept) sends
// nothing, so Outlook never gets it twice and Google never gets an echo.
func TestTheSweepsOwnReadAsksForNoSyncEntry(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	_, sweep := syncWithGoogle(t, h, sam, g)
	g.pushed()
	synced := drainAtOnce(t, h)
	start := h.clock.Add(48 * time.Hour)
	g.put(calendarSyncBusiness.Change{ExternalID: "bk", Event: ICS.Event{UID: "booked@google.com", Summary: "Booked at Google", Start: start, End: start.Add(time.Hour), Zone: "UTC"}})
	sweep()
	if eventFromGoogle(h, "booked@google.com") == nil {
		t.Fatal("Google's new event didn't come")
	}
	if len(*synced) != 0 {
		t.Fatalf("the sweep's own read asked for sync entries %v", *synced)
	}
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("Google's own new event was sent back: %v", p)
	}
}

// A person's own .ics import still asks for both: its events are new here.
func TestAnIcsImportStillAsksForSyncEntries(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	syncWithGoogle(t, h, sam, g)
	g.pushed()
	synced := drainAtOnce(t, h)
	start := h.clock.Add(48 * time.Hour)
	if _, err := h.s.importCalendars(context.Background(), sam, []ICS.Calendar{{Name: "Mine", Events: []ICS.Event{{UID: "mine@file", Summary: "From a file", Start: start, End: start.Add(time.Hour), Zone: "UTC"}}}}, func(int, int) {}); err != nil {
		t.Fatal(err)
	}
	if len(*synced) != 1 {
		t.Fatalf("a file's import asked for sync entries %v, want one", *synced)
	}
}

// CR-383 F4: the sink holds what the sweep holds. A calendar not read whole
// yet (no cursor: a new link, or a move that asked for a whole read) gets
// nothing from the sink; the sweep sends once its whole read has worked.
func TestTheSyncSinkSendsNothingToACalendarNotYetReadWhole(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	ledger, sweep := syncWithGoogle(t, h, sam, g)
	g.pushed()
	ledger.readWhole()
	e := h.create(sam, "Pricing review")
	if err := h.s.runSyncSink(context.Background(), syncDelivery(t, e.ID)); err != nil {
		t.Fatal(err)
	}
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("the sink sent to a calendar not read whole: %v", p)
	}
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"create Pricing review"}) {
		t.Fatalf("after its whole read, the sweep sent %v", p)
	}
}

// CR-383 F3: on the sink's path, an instance that hits a passing failure
// after its series was sent is kept as a refusal (the one ledger), so a
// later run sends it though the series' ref is stamped past it.
func TestAnInstanceTheSinkCouldntSendIsSentLater(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	g.put(calendarSyncBusiness.Change{ExternalID: "st", Event: ICS.Event{UID: "standup@google.com", Summary: "Standup", Start: start, End: start.Add(15 * time.Minute), Zone: "Europe/London", RRule: "FREQ=DAILY;COUNT=5"}})
	_, sweep := syncWithGoogle(t, h, sam, g)
	rid := start.AddDate(0, 0, 2)
	g.put(calendarSyncBusiness.Change{ExternalID: "st_" + rid.Format("20060102T150405Z"), SeriesID: "st", Event: ICS.Event{UID: "standup@google.com", RecurrenceID: &rid, Summary: "Standup", Start: rid.Add(time.Hour), End: rid.Add(time.Hour + 15*time.Minute), Zone: "Europe/London"}})
	sweep()
	series := eventFromGoogle(h, "standup@google.com")
	ov, err := h.store.overrideAt(context.Background(), series.ID, rid)
	if err != nil || ov == nil {
		t.Fatalf("Google's instance didn't come: %v", err)
	}
	g.pushed()
	down := true
	g.refuse = func(op calendarSyncBusiness.Op) error {
		if down && op.Event.RecurrenceID != nil {
			return errors.New("503 backend error")
		}
		return nil
	}
	h.clock = h.clock.Add(time.Minute)
	h.store.mu.Lock()
	h.store.events[ov.ID].Title, h.store.events[ov.ID].UpdatedAt = "Standup, late", h.clock
	h.store.mu.Unlock()
	if err := h.s.runSyncSink(context.Background(), syncDelivery(t, series.ID.String())); err == nil {
		t.Fatal("a 503 for the instance didn't ask the outbox to retry")
	}
	g.pushed()
	down = false
	h.clock = h.clock.Add(syncRetries[0] + syncRetries[0]/4)
	sweep()
	if p := g.pushed(); !slices.Contains(p, "update Standup, late") {
		t.Fatalf("the instance the sink couldn't send was never sent: %v", p)
	}
}

// The test gap (CR-383): the sink sends only the link's person's events. On
// a calendar Sam's link maps, an event Bea made is never sent to Sam's
// Google, by the sink or by the sweep.
func TestTheSyncSinkNeverSendsAnotherPersonsEvent(t *testing.T) {
	h := newHarness(t)
	sam, bea := h.member("Sam"), h.member("Bea")
	g := newLiveGoogle(func() time.Time { return h.clock })
	_, sweep := syncWithGoogle(t, h, sam, g)
	g.pushed()
	own, _ := h.store.personalCalendar(context.Background(), personOf(sam), "UTC")
	beas := h.onCalendar(own, bea, "Bea's call", h.clock.Add(48*time.Hour), "")
	mine := h.onCalendar(own, sam, "Sam's call", h.clock.Add(50*time.Hour), "")
	for _, id := range []uuid.UUID{beas.ID, mine.ID} {
		if err := h.s.runSyncSink(context.Background(), syncDelivery(t, id.String())); err != nil {
			t.Fatal(err)
		}
	}
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"create Sam's call"}) {
		t.Fatalf("Sam's link sent %v; want Sam's own event only", p)
	}
}

// N1: a link the sweep is sending now is left to it: the sink answers nil
// (no attempt spent, nothing driven dead), and the sweep sends the change.
func TestTheSyncSinkLeavesALinkBeingSentToTheSweep(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	ledger, sweep := syncWithGoogle(t, h, sam, g)
	g.pushed()
	e := h.create(sam, "Pricing review")
	l := googleLinkOf(t, ledger, sam)
	if !pushing.Acquire(l.ID.String(), 1) {
		t.Fatal("the link's lock was taken")
	}
	err := h.s.runSyncSink(context.Background(), syncDelivery(t, e.ID))
	pushing.Release(l.ID.String())
	if err != nil {
		t.Fatalf("a link being sent spent an attempt: %v", err)
	}
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("the sink sent while the sweep was sending: %v", p)
	}
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"create Pricing review"}) || strings.Contains(strings.Join(p, ","), "Pricing review,") {
		t.Fatalf("the sweep sent %v", p)
	}
}
