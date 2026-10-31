package Calendar

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	ICS "github.com/akashc777/OneCamp/business/ICS"
)

// An export holds every event the person is on, as the calendar shows it,
// and imports into another calendar as the same instances (SY-18).
func TestAnExportImportsAsTheSameCalendar(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	h.people[personOf(sam).String()].EmailID = "sam@work.example"
	h.store.addresses["sam@work.example"] = personOf(sam)
	if _, err := h.s.importCalendars(context.Background(), sam, readFile(t, googleFile("jonas@example.com")), func(int, int) {}); err != nil {
		t.Fatal(err)
	}
	// And one made here, with Maya on it.
	made := h.create(sam, "Standup", maya)

	var out bytes.Buffer
	n, err := h.s.Export(context.Background(), sam, &out)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("exported %d events", n)
	}
	cal, err := ICS.Read(bytes.NewReader(out.Bytes()))
	if err != nil || len(cal.Skipped) != 0 {
		t.Fatalf("read back: %v %v\n%s", err, cal.Skipped, out.String())
	}
	var review, standup *ICS.Event
	for i := range cal.Events {
		switch cal.Events[i].UID {
		case "review@google.com":
			if cal.Events[i].RecurrenceID == nil {
				review = &cal.Events[i]
			}
		case made.ICalUID:
			standup = &cal.Events[i]
		}
	}
	if review == nil || standup == nil {
		t.Fatalf("events %+v", cal.Events)
	}
	if review.Zone != "Europe/London" || review.Description != "Agenda\nPricing" || len(review.ExDates) != 1 || len(review.Alarms) != 1 || review.Alarms[0].Before != 10*time.Minute {
		t.Fatalf("review %+v", review)
	}
	var emails []string
	for _, a := range review.Attendees {
		emails = append(emails, a.Email+"="+a.PartStat)
	}
	slices.Sort(emails)
	if strings.Join(emails, " ") != "jonas@example.com=ACCEPTED lee@example.com=DECLINED maya@example.com=ACCEPTED" {
		t.Fatalf("guests %v", emails)
	}
	if standup.Organizer.Email != "sam@work.example" {
		t.Fatalf("standup %+v", standup)
	}

	// Maya imports Sam's export: the review is hers now, with the same
	// instances; the standup she is on already.
	in, err := h.s.importCalendars(context.Background(), maya, []ICS.Calendar{cal}, func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Created) != 2 || len(in.Skipped) != 0 || in.Unchanged != 1 {
		t.Fatalf("Maya's import %+v", in)
	}
	starts := func(evs []Event) []string {
		var out []string
		for _, e := range evs {
			if strings.HasPrefix(e.Title, "Weekly design review") {
				out = append(out, e.Start.At.UTC().Format(time.RFC3339))
			}
		}
		return out
	}
	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	samEvents, _, err := h.s.Range(context.Background(), sam, from, to)
	if err != nil {
		t.Fatal(err)
	}
	mayaEvents, _, err := h.s.Range(context.Background(), maya, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if s, m := starts(samEvents), starts(mayaEvents); len(s) != 7 || !slices.Equal(s, m) {
		t.Fatalf("Sam's %v, Maya's %v", s, m)
	}
}

func TestAnImportedCopyExportsUnderTheUIDItCopies(t *testing.T) {
	if got := exportedUID("review@google.com~" + "6f1c2a4e-0c7b-4a0e-9d3f-2b8a1c5e7d90"); got != "review@google.com" {
		t.Fatal(got)
	}
	if got := exportedUID("odd~uid"); got != "odd~uid" {
		t.Fatal(got)
	}
}
