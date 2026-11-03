package Calendar

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/business/CalendarSync/icsurl"
)

// A read still in flight when its calendar is unsubscribed (the subscribe's
// own background read, or the sweep's) leaves nothing on the kept calendar,
// and subscribing to the address again brings every event onto the new one,
// though the late read kept where its events were under the same address
// (V-184 head 5c, CODE-REVIEWER-161's F1).
func TestAReadInFlightAtUnsubscribeLeavesNothingAndSubscribingAgainBringsEverything(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	_, ledger := withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)})
	ctx := context.Background()
	v, err := h.s.Subscribe(ctx, sam, teamAddress, "", "")
	if err != nil {
		t.Fatal(err)
	}
	old := uuid.MustParse(v.CalendarID)
	want := len(onCalendar(h, old))
	if want == 0 || len(ledger.subs) != 1 {
		t.Fatalf("subscribed: %d events, %d subscriptions; want some and one", want, len(ledger.subs))
	}
	sub := ledger.subs[0]
	file, cursor, err := readAddress(ctx, teamAddress)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Unsubscribe(ctx, sam, v.CalendarID); err != nil {
		t.Fatal(err)
	}
	// The read in flight writes now, after the unsubscribe.
	h.s.takeSubscribed(ctx, sub, icsurl.ChangesOf(file), cursor)
	if left := onCalendar(h, old); len(left) != 0 {
		t.Fatalf("a read in flight left %d events on the unsubscribed calendar; want none", len(left))
	}
	again, err := h.s.Subscribe(ctx, sam, teamAddress, "", "")
	if err != nil {
		t.Fatalf("subscribed again: %v", err)
	}
	if got := len(onCalendar(h, uuid.MustParse(again.CalendarID))); got != want {
		t.Fatalf("subscribed again: %d events on the new calendar; want all %d", got, want)
	}
}

// A late read's refs can still stand when a new subscription to the address is
// read: its own forget runs on its way out, which can come after the new read.
// Those refs have the same link, remote and etag as the new read's events, but
// name the late read's copies on the unsubscribed calendar. A ref counts only
// for an event on the calendar being read, so the new calendar still gets
// every event, and the late copies go when the held forget runs (V-184 head
// 5c, CODE-REVIEWER-161's F1 (b)).
func TestALateReadsRefsKeepNothingOffANewSubscription(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	_, ledger := withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)})
	ctx := context.Background()
	v, err := h.s.Subscribe(ctx, sam, teamAddress, "", "")
	if err != nil {
		t.Fatal(err)
	}
	old := uuid.MustParse(v.CalendarID)
	want := len(onCalendar(h, old))
	sub := ledger.subs[0]
	file, cursor, err := readAddress(ctx, teamAddress)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Unsubscribe(ctx, sam, v.CalendarID); err != nil {
		t.Fatal(err)
	}
	h.store.holdForget(true)
	h.s.takeSubscribed(ctx, sub, icsurl.ChangesOf(file), cursor)
	if late := len(onCalendar(h, old)); want == 0 || late != want {
		t.Fatalf("the late read wrote %d events onto the unsubscribed calendar; want all %d, with their refs", late, want)
	}
	h.store.holdForget(false)
	again, err := h.s.Subscribe(ctx, sam, teamAddress, "", "")
	if err != nil {
		t.Fatalf("subscribed again: %v", err)
	}
	if got := len(onCalendar(h, uuid.MustParse(again.CalendarID))); got != want {
		t.Fatalf("subscribed again while a late read's refs stood: %d events on the new calendar; want all %d", got, want)
	}
	if err := h.store.forgetIfUnsubscribed(ctx, old); err != nil {
		t.Fatal(err)
	}
	if left := len(onCalendar(h, old)); left != 0 {
		t.Fatalf("the held forget left %d events on the unsubscribed calendar; want none", left)
	}
}
