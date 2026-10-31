package Calendar

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// A member an event isn't shared with, who doesn't manage its calendar, is
// refused every call that names it, the same way an unknown id is, so the
// answer never says it is there; and their own lists show nothing of it.
// router/testdata/route-access.tsv names this test for the calendar routes
// that take an event's or a calendar's id.
func TestSomeoneElsesEventOrCalendarIsRefusedEveryCall(t *testing.T) {
	h := newHarness(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Board prep", Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := h.s.Create(ctx, jonas, EventInput{Title: "Old offsite", Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Delete(ctx, jonas, gone.ID, "", &gone.Sequence); err != nil {
		t.Fatal(err)
	}
	own, err := h.s.PersonCalendar(ctx, jonas, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	seq, title, name := ev.Sequence, "Mine now", "Mine now"

	for _, c := range []struct {
		what string
		call func() error
	}{
		{"read his event", func() error { _, err := h.s.Get(ctx, maya, ev.ID); return err }},
		{"change it", func() error {
			_, err := h.s.Update(ctx, maya, ev.ID, EventPatch{Sequence: &seq, Title: &title})
			return err
		}},
		{"delete it", func() error { return h.s.Delete(ctx, maya, ev.ID, "", &seq) }},
		{"restore one he deleted", func() error { _, err := h.s.Restore(ctx, maya, gone.ID); return err }},
		{"answer it", func() error { _, err := h.s.Answer(ctx, maya, ev.ID, AnswerInput{Answer: Accepted}); return err }},
		{"nudge its guests", func() error { _, err := h.s.Nudge(ctx, maya, ev.ID); return err }},
		{"make a guest's link to it", func() error { _, err := h.s.GuestLink(ctx, maya, ev.ID, "ana@example.test"); return err }},
		{"give it a call", func() error { _, err := h.s.AddCall(ctx, maya, ev.ID); return err }},
		{"take its call off", func() error { return h.s.RemoveCall(ctx, maya, ev.ID) }},
		{"read its reminders", func() error { _, err := h.s.Reminders(ctx, maya, ev.ID); return err }},
		{"set its reminders", func() error { _, err := h.s.SetReminders(ctx, maya, ev.ID, []int{10}); return err }},
		{"snooze its reminder", func() error { _, err := h.s.Snooze(ctx, maya, ev.ID, SnoozeFiveMinutes); return err }},
		{"list who his calendar is shared with", func() error { _, err := h.s.Shares(ctx, maya, own.String()); return err }},
		{"share his calendar", func() error {
			return h.s.Share(ctx, maya, own.String(), personOf(maya).String(), models.ShareDetails)
		}},
		{"rename his calendar", func() error { return h.s.UpdateCalendar(ctx, maya, own.String(), CalendarPatch{Name: &name}) }},
	} {
		if err := c.call(); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("Maya tried to %s: %v, want it refused as not there", c.what, err)
		}
	}

	var cant *EventInputError
	if _, err := h.s.Create(ctx, maya, EventInput{Title: "Mine now", Start: start, End: end, CalendarID: own.String()}); !errors.As(err, &cant) {
		t.Errorf("Maya put an event on Jonas's calendar: %v, want it refused", err)
	}
	_, _ = withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)})
	sub, err := h.s.Subscribe(ctx, jonas, teamAddress, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.RefreshSubscription(ctx, maya, sub.CalendarID); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("Maya refreshed Jonas's subscription: %v, want it refused as not there", err)
	}

	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	seen, _, err := h.s.Range(ctx, maya, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range seen {
		if e.ID == ev.ID {
			t.Error("Maya's own range holds Jonas's event")
		}
	}
	trash, err := h.s.Trash(ctx, maya, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trash {
		if e.ID == gone.ID {
			t.Error("Maya's trash holds the event Jonas deleted")
		}
	}
	var ics bytes.Buffer
	if _, err := h.s.Export(ctx, maya, &ics); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ics.String(), "Board prep") {
		t.Error("Maya's export holds Jonas's event")
	}
	cals, err := h.s.Calendars(ctx, maya)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cals {
		if c.ID == own.String() {
			t.Error("Maya's calendars list Jonas's own, which he never shared")
		}
	}
}
