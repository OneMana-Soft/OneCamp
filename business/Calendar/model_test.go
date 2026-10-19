package Calendar

import (
	"errors"
	"strings"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// The zones every calendar time test runs in (PROGRAM.md G-CAL-TZ), named
// explicitly: Go never reads the machine's.
var tzMatrix = []string{"UTC", "America/New_York", "Asia/Kolkata", "Europe/London", "Pacific/Chatham", "America/St_Johns"}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// The days the clocks change in New York and London in 2026, and an ordinary
// day, as dates.
var clockDays = []string{"2026-03-08", "2026-11-01", "2026-03-29", "2026-10-25", "2026-10-16"}

// An all-day event written as instants is the midnights of whole days in its
// zone: in every zone of the matrix, on every day the clocks change, it reads
// back as its dates; anything else is a timed event.
func TestWholeDaysAreTheirDatesInEveryZone(t *testing.T) {
	for _, name := range tzMatrix {
		loc := zone(t, name)
		for _, day := range clockDays {
			d, _ := time.ParseInLocation(dateLayout, day, loc)
			start, end := d, d.AddDate(0, 0, 1)
			first, after, ok := datesIn(start.UTC(), end.UTC(), loc)
			if !ok || first != day || after != end.Format(dateLayout) {
				t.Errorf("%s %s: got %q %q %v, want the day itself", name, day, first, after, ok)
			}
			// Three whole days.
			if f, a, ok := datesIn(start.UTC(), d.AddDate(0, 0, 3).UTC(), loc); !ok || f != day || a != d.AddDate(0, 0, 3).Format(dateLayout) {
				t.Errorf("%s %s: three days read as %q %q %v", name, day, f, a, ok)
			}
			// Not midnights there: a timed event.
			if _, _, ok := datesIn(start.Add(time.Hour).UTC(), end.UTC(), loc); ok {
				t.Errorf("%s %s: 01:00 to midnight read as whole days", name, day)
			}
			// No length at all: not a day.
			if _, _, ok := datesIn(start.UTC(), start.UTC(), loc); ok {
				t.Errorf("%s %s: a moment read as whole days", name, day)
			}
		}
	}
}

// The web sends an all-day event at its reader's own midnights. Wherever the
// reader is, the dates they meant come back, the end's offset following a
// clock change; when two offsets fit (the far east and west), the dates
// nearer the event's own are taken.
func TestAReadersMidnightsAreTheDatesTheyMeant(t *testing.T) {
	for _, name := range tzMatrix {
		loc := zone(t, name)
		for _, day := range clockDays {
			d, _ := time.ParseInLocation(dateLayout, day, loc)
			next := d.AddDate(0, 0, 1)
			first, after, ok := datesAtAnyOffset(d.UTC(), next.UTC(), day)
			if !ok || first != day || after != next.Format(dateLayout) {
				t.Errorf("%s %s: got %q %q %v", name, day, first, after, ok)
			}
		}
	}
	// 10:15 UTC is a midnight at +13:45 (Chatham, the 16th) and at −10:15
	// (the 15th): the date nearer the event's own wins.
	start := time.Date(2026, 10, 15, 10, 15, 0, 0, time.UTC)
	if f, _, ok := datesAtAnyOffset(start, start.Add(24*time.Hour), "2026-10-16"); !ok || f != "2026-10-16" {
		t.Errorf("near the 16th: %q", f)
	}
	if f, _, ok := datesAtAnyOffset(start, start.Add(24*time.Hour), "2026-10-14"); !ok || f != "2026-10-15" {
		t.Errorf("near the 14th: %q", f)
	}
	// 10:07 is a midnight nowhere.
	if _, _, ok := datesAtAnyOffset(start.Add(-8*time.Minute), start.Add(24*time.Hour), "2026-10-16"); ok {
		t.Error("10:07 UTC read as somebody's midnight")
	}
}

// legacyWhen: the times an /event route sent become dates or instants, in
// the event's zone, and an all-day event moved by someone elsewhere stays
// all-day; focus time is always instants.
func TestTheOldRoutesTimesBecomeDatesOrInstants(t *testing.T) {
	kolkata, newYork := zone(t, "Asia/Kolkata"), zone(t, "America/New_York")
	istMidnight := time.Date(2026, 10, 16, 0, 0, 0, 0, kolkata)

	e := blank("Asia/Kolkata")
	legacyWhen(e, istMidnight, istMidnight.AddDate(0, 0, 1), nil)
	if !e.AllDay() || e.StartDate != "2026-10-16" || e.EndDate != "2026-10-17" || e.StartAt != nil {
		t.Errorf("a whole day in its zone: %+v", e)
	}

	// The same instants for someone whose zone is unknown (UTC): timed, as
	// before the store.
	u := blank("UTC")
	legacyWhen(u, istMidnight, istMidnight.AddDate(0, 0, 1), nil)
	if u.AllDay() || !u.StartAt.Equal(istMidnight) {
		t.Errorf("a zone not known: %+v", u)
	}

	// Dragged in New York to the 20th: New York's midnights, still all-day.
	nyMidnight := time.Date(2026, 10, 20, 0, 0, 0, 0, newYork)
	moved := blank("Asia/Kolkata")
	legacyWhen(moved, nyMidnight, nyMidnight.AddDate(0, 0, 1), e)
	if !moved.AllDay() || moved.StartDate != "2026-10-20" || moved.EndDate != "2026-10-21" {
		t.Errorf("moved from New York: %+v", moved)
	}

	// Dragged into the day's hours: timed.
	timed := blank("Asia/Kolkata")
	legacyWhen(timed, nyMidnight.Add(9*time.Hour), nyMidnight.Add(10*time.Hour), e)
	if timed.AllDay() || !timed.StartAt.Equal(nyMidnight.Add(9*time.Hour)) {
		t.Errorf("into the hours: %+v", timed)
	}

	focus := blank("Asia/Kolkata")
	focus.Kind = string(EventFocus)
	legacyWhen(focus, istMidnight, istMidnight.AddDate(0, 0, 1), nil)
	if focus.AllDay() {
		t.Error("focus time became whole days")
	}
}

// Away beats focus; a flag left out leaves the kind; a flag turned off makes
// that kind a plain event, and leaves any other kind alone.
func TestTheFocusAndAwayFlagsKeepTheirMeaning(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		cur          EventKind
		focus, away  *bool
		want         EventKind
		whatItProves string
	}{
		{EventPlain, nil, nil, EventPlain, "nothing sent"},
		{EventPlain, &yes, nil, EventFocus, "focus on"},
		{EventPlain, nil, &yes, EventAway, "away on"},
		{EventPlain, &yes, &yes, EventAway, "away beats focus"},
		{EventFocus, nil, &yes, EventAway, "away turns focus off"},
		{EventAway, &yes, nil, EventFocus, "focus turns away off"},
		{EventFocus, &no, nil, EventPlain, "focus off"},
		{EventAway, nil, &no, EventPlain, "away off"},
		{EventFocus, nil, nil, EventFocus, "left out"},
		{EventBooking, &no, &no, EventBooking, "off leaves another kind"},
	} {
		if got := legacyKind(c.cur, c.focus, c.away); got != c.want {
			t.Errorf("%s: %s, want %s", c.whatItProves, got, c.want)
		}
	}
}

// setWhen takes instants with zones or dates, never a mix, never an end
// before the start, never a zone it can't read.
func TestAnEventsTimeIsZonedOrFloating(t *testing.T) {
	start := time.Date(2026, 10, 16, 9, 15, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	var input *EventInputError
	for _, c := range []struct {
		name       string
		start, end When
		ok         bool
	}{
		{"timed", When{At: &start, Zone: "Europe/London"}, When{At: &end}, true},
		{"a flight", When{At: &start, Zone: "Europe/London"}, When{At: &end, Zone: "Asia/Kolkata"}, true},
		{"all day", When{Date: "2026-10-16"}, When{Date: "2026-10-17"}, true},
		{"end first", When{At: &end}, When{At: &start}, false},
		{"a mix", When{Date: "2026-10-16"}, When{At: &end}, false},
		{"no end", When{At: &start}, When{}, false},
		{"a day that isn't", When{Date: "2026-02-30"}, When{Date: "2026-03-01"}, false},
		{"ends on its start", When{Date: "2026-10-16"}, When{Date: "2026-10-16"}, false},
		{"no such zone", When{At: &start, Zone: "Mars/Olympus"}, When{At: &end}, false},
		{"the server's zone", When{At: &start, Zone: "Local"}, When{At: &end}, false},
	} {
		e := blank("UTC")
		err := setWhen(e, c.start, c.end)
		if (err == nil) != c.ok || (err != nil && !errors.As(err, &input)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	e := blank("UTC")
	_ = setWhen(e, When{At: &start, Zone: "Europe/London"}, When{At: &end, Zone: "Asia/Kolkata"})
	if e.TZ != "Europe/London" || e.EndTZ != "Asia/Kolkata" {
		t.Errorf("zones %q %q", e.TZ, e.EndTZ)
	}
	s, en := whenOf(e)
	if s.Zone != "Europe/London" || en.Zone != "Asia/Kolkata" || !s.At.Equal(start) {
		t.Errorf("read back %+v %+v", s, en)
	}
}

// An all-day event takes the whole of its days in its own zone, whatever
// zone reads it: midnight there to midnight there, across a clock change.
func TestAnAllDayEventsStretchIsItsZonesMidnights(t *testing.T) {
	e := blank("America/New_York")
	e.StartDate, e.EndDate = "2026-11-01", "2026-11-02"
	start, end := spanOf(e)
	if !start.Equal(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("1 November in New York: %v to %v", start, end)
	}
	if got := end.Sub(start); got != 25*time.Hour {
		t.Errorf("the day the clocks go back is %v long, want 25h", got)
	}
}

// What one event holds is capped on the server (BE-F-15), and the kinds and
// settings are only the ones there are.
func TestAnEventsWordsAndSettingsAreChecked(t *testing.T) {
	ok := func() *models.Event { return blank("UTC") }
	for _, c := range []struct {
		name string
		edit func(*models.Event)
	}{
		{"title", func(e *models.Event) { e.Title = strings.Repeat("a", 301) }},
		{"notes", func(e *models.Event) { e.Notes = strings.Repeat("a", 100001) }},
		{"location", func(e *models.Event) { e.Location = strings.Repeat("a", 1001) }},
		{"kind", func(e *models.Event) { e.Kind = "party" }},
		{"notes format", func(e *models.Event) { e.NotesFormat = "rtf" }},
		{"show as", func(e *models.Event) { e.Transparency = "maybe" }},
		{"visibility", func(e *models.Event) { e.Visibility = "secret" }},
		{"colour", func(e *models.Event) { e.Hue = "clay" }},
	} {
		e := ok()
		start, end := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
		e.StartAt, e.EndAt = &start, &end
		c.edit(e)
		if err := check(e); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	e := ok()
	start, end := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	e.StartAt, e.EndAt = &start, &end
	e.Title, e.Hue = strings.Repeat("é", 300), "berry"
	if err := check(e); err != nil {
		t.Errorf("300 characters and a camp hue: %v", err)
	}
}

// A read covers 400 days at most, never ends before it starts, and reads
// every date that could touch it anywhere: 12 hours before its start's date
// to 14 hours after its end's.
func TestAReadsWindowIsBounded(t *testing.T) {
	from := time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)
	if _, err := window(from, from.AddDate(0, 0, 401)); err == nil {
		t.Error("401 days read")
	}
	if _, err := window(from, from.Add(-time.Second)); err == nil {
		t.Error("a window ending before it starts read")
	}
	w, err := window(from, from.AddDate(0, 0, 400))
	if err != nil {
		t.Fatal(err)
	}
	if w.FromDate != "2026-10-15" || w.ToDate != "2027-11-20" {
		t.Errorf("dates %s to %s", w.FromDate, w.ToDate)
	}
}

// A change names what changed, and only that.
func TestAChangeNamesWhatChanged(t *testing.T) {
	a := blank("UTC")
	start, end := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	a.StartAt, a.EndAt, a.Title = &start, &end, "Stand-up"
	b := copyEvent(a)
	if got := changedFields(a, b, false); len(got) != 0 {
		t.Errorf("nothing changed: %v", got)
	}
	later := end.Add(time.Hour)
	b.EndAt, b.Title = &later, "Stand-up (long)"
	if got := strings.Join(changedFields(a, b, true), ","); got != "title,time,guests" {
		t.Errorf("got %s", got)
	}
}
