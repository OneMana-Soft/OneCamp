package Calendar

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	ics "github.com/akashc777/OneCamp/business/ICS"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// A Google Calendar export of Sam's: a weekly review Jonas organises in
// London, one Tuesday skipped and one moved, with Maya and Sam on it and an
// alarm; and a day off.
func googleFile(organiser string) string {
	return strings.Join([]string{
		"BEGIN:VCALENDAR", "PRODID:-//Google Inc//Google Calendar 70.9054//EN", "VERSION:2.0", "X-WR-CALNAME:sam@work.example",
		"BEGIN:VEVENT",
		"DTSTART;TZID=Europe/London:20261006T091500", "DTEND;TZID=Europe/London:20261006T100000",
		"RRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=8", "EXDATE;TZID=Europe/London:20261020T091500",
		"UID:review@google.com", "SUMMARY:Weekly design review", "DESCRIPTION:Agenda\\nPricing", "LOCATION:Room 4",
		"ORGANIZER;CN=Jonas:mailto:" + organiser,
		"ATTENDEE;CN=Maya Chen;PARTSTAT=ACCEPTED:mailto:maya@example.com",
		"ATTENDEE;CN=Sam;PARTSTAT=ACCEPTED:mailto:SAM@work.example",
		"ATTENDEE;CN=Lee;ROLE=OPT-PARTICIPANT;PARTSTAT=DECLINED:mailto:lee@example.com",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT10M", "END:VALARM",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"DTSTART;TZID=Europe/London:20261028T140000", "DTEND;TZID=Europe/London:20261028T144500",
		"RECURRENCE-ID;TZID=Europe/London:20261027T091500",
		"UID:review@google.com", "SUMMARY:Weekly design review (moved)", "DESCRIPTION:Agenda\\nPricing", "LOCATION:Room 4",
		"END:VEVENT",
		"BEGIN:VEVENT", "DTSTART;VALUE=DATE:20261224", "DTEND;VALUE=DATE:20261227", "UID:off@google.com", "SUMMARY:Day off", "TRANSP:TRANSPARENT", "END:VEVENT",
		"END:VCALENDAR", ""}, "\r\n")
}

func readFile(t *testing.T, file string) []ics.Calendar {
	t.Helper()
	cal, err := ics.Read(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	return []ics.Calendar{cal}
}

func TestAGoogleExportComesInAsTheSameCalendar(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	h.store.addresses["sam@work.example"] = personOf(sam)
	var progress []int
	made, err := h.s.importCalendars(context.Background(), sam, readFile(t, googleFile("jonas@example.com")), func(done, total int) { progress = append(progress, done) })
	if err != nil {
		t.Fatal(err)
	}
	if len(made.Created) != 2 || len(made.Skipped) != 0 {
		t.Fatalf("made %+v", made)
	}
	if progress[len(progress)-1] != 2 {
		t.Fatalf("progress %v", progress)
	}
	series := h.store.events[uuid.MustParse(made.Created[0].UUID)]
	if series.ICalUID != "review@google.com" || series.Source != sourceICS || series.CreatedBy != personOf(sam) || series.TZ != "Europe/London" {
		t.Fatalf("series %+v", series)
	}
	if !strings.HasPrefix(series.RRule, "FREQ=WEEKLY") || len(series.Exdates) != 1 || !series.Exdates[0].Equal(time.Date(2026, 10, 20, 8, 15, 0, 0, time.UTC)) {
		t.Fatalf("rule %q exdates %v", series.RRule, series.Exdates)
	}
	if series.Notes != "Agenda\nPricing" || series.Location != "Room 4" || !series.StartAt.Equal(time.Date(2026, 10, 6, 8, 15, 0, 0, time.UTC)) {
		t.Fatalf("fields %+v", series)
	}
	// The moved Tuesday is its own row, changed in its time and title.
	var moved *models.Event
	for _, e := range h.store.events {
		if e.SeriesID != nil && *e.SeriesID == series.ID {
			moved = e
		}
	}
	if moved == nil || !moved.RecurrenceID.Equal(time.Date(2026, 10, 27, 9, 15, 0, 0, time.UTC)) || !slices.Contains(moved.Changed, "time") || !slices.Contains(moved.Changed, "title") {
		t.Fatalf("moved instance %+v", moved)
	}
	// Guests by email with their answers, the organiser as a yes, never Sam,
	// and nobody invited or told.
	guests := h.store.guests[series.ID]
	got := map[string]string{}
	for _, g := range guests {
		if g.UserID != nil || g.InviteState != models.InviteImported {
			t.Fatalf("guest %+v", g)
		}
		got[g.Email] = g.Response + "/" + g.Role
	}
	want := map[string]string{"jonas@example.com": "accepted/required", "maya@example.com": "accepted/required", "lee@example.com": "declined/optional"}
	if len(got) != len(want) {
		t.Fatalf("guests %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("guests %v, want %v", got, want)
		}
	}
	if len(h.tells.tells) != 0 || len(h.emitted) != 0 {
		t.Fatalf("an import told people: %d tellings, %d events", len(h.tells.tells), len(h.emitted))
	}
	// The alarm is Sam's reminder.
	mins, _ := h.s.rem.remindersOf(context.Background(), series.ID, []uuid.UUID{personOf(sam)})
	if !slices.Equal(mins[personOf(sam)], []int{10}) {
		t.Fatalf("reminders %v", mins)
	}
	// The day off is all-day and shows as free.
	off := h.store.events[uuid.MustParse(made.Created[1].UUID)]
	if off.StartDate != "2026-12-24" || off.EndDate != "2026-12-27" || off.Transparency != transparencyTransparent {
		t.Fatalf("day off %+v", off)
	}
	// It reads back on Sam's calendar as the series it was: the skipped
	// Tuesday gone, the moved one on Wednesday.
	evs, _, err := h.s.Range(context.Background(), sam, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var starts []string
	for _, e := range evs {
		if e.Title != "Day off" {
			starts = append(starts, e.Start.At.In(londonZone(t)).Format("Mon 2 Jan 15:04"))
		}
	}
	wantStarts := []string{"Tue 6 Oct 09:15", "Tue 13 Oct 09:15", "Wed 28 Oct 14:00", "Tue 3 Nov 09:15", "Tue 10 Nov 09:15", "Tue 17 Nov 09:15", "Tue 24 Nov 09:15"}
	if !slices.Equal(starts, wantStarts) {
		t.Fatalf("instances %v, want %v", starts, wantStarts)
	}
}

func londonZone(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Importing a file twice makes nothing twice; an event someone else in the
// workspace has comes in as the importer's own copy; an event made here
// that the importer is on is left out.
func TestImportingTwiceMakesNothingTwice(t *testing.T) {
	h := newHarness(t)
	sam, jonas := h.member("Sam"), h.member("Jonas")
	file := googleFile("jonas@example.com")
	// Jonas imported the same review first.
	if made, err := h.s.importCalendars(context.Background(), jonas, readFile(t, file), func(int, int) {}); err != nil || len(made.Created) != 2 {
		t.Fatalf("Jonas's import: %+v %v", made, err)
	}
	made, err := h.s.importCalendars(context.Background(), sam, readFile(t, file), func(int, int) {})
	if err != nil || len(made.Created) != 2 {
		t.Fatalf("Sam's import: %+v %v", made, err)
	}
	if e := h.store.events[uuid.MustParse(made.Created[0].UUID)]; e.ICalUID != "review@google.com~"+e.CalendarID.String() || e.CreatedBy != personOf(sam) {
		t.Fatalf("Sam's copy %+v", e)
	}
	again, err := h.s.importCalendars(context.Background(), sam, readFile(t, file), func(int, int) {})
	if err != nil || len(again.Created) != 0 || len(again.Skipped) != 0 || again.Unchanged != 2 {
		t.Fatalf("again: %+v %v", again, err)
	}
	// An event made here, exported and imported back, is there already.
	mine := h.create(sam, "Standup")
	back := strings.Join([]string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "UID:" + mine.ICalUID, "SUMMARY:Standup", "DTSTART:20261020T090000Z", "DTEND:20261020T091500Z", "END:VEVENT", "END:VCALENDAR", ""}, "\r\n")
	if made, _ := h.s.importCalendars(context.Background(), sam, readFile(t, back), func(int, int) {}); len(made.Created) != 0 || len(made.Skipped) != 0 || made.Unchanged != 1 {
		t.Fatalf("an event made here: %+v", made)
	}
}

// What can't come in is left out with its reason; the rest comes in.
func TestAnEventThatCantComeInIsLeftOutWithItsReason(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	file := strings.Join([]string{"BEGIN:VCALENDAR",
		"BEGIN:VEVENT", "UID:ok", "SUMMARY:Fine", "DTSTART:20261020T090000Z", "DTEND:20261020T100000Z", "END:VEVENT",
		"BEGIN:VEVENT", "UID:rule", "SUMMARY:Odd rule", "DTSTART:20261020T090000Z", "DTEND:20261020T100000Z", "RRULE:FREQ=FORTNIGHTLY", "END:VEVENT",
		"BEGIN:VEVENT", "UID:gone", "SUMMARY:Called off", "STATUS:CANCELLED", "DTSTART:20261020T090000Z", "END:VEVENT",
		"BEGIN:VEVENT", "UID:ok", "SUMMARY:Fine again", "DTSTART:20261021T090000Z", "END:VEVENT",
		"END:VCALENDAR", ""}, "\r\n")
	made, err := h.s.importCalendars(context.Background(), sam, readFile(t, file), func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(made.Created) != 1 || made.Created[0].Title != "Fine" {
		t.Fatalf("created %+v", made.Created)
	}
	var whys []string
	for _, s := range made.Skipped {
		whys = append(whys, s.What+": "+s.Why)
	}
	all := strings.Join(whys, "\n")
	for _, want := range []string{"Fine again: It appears twice", "Odd rule: Its repeat couldn't be read", "Called off: It was cancelled."} {
		if !strings.Contains(all, want) {
			t.Errorf("skipped %q, want %q", all, want)
		}
	}
}

// A zip of calendar files (Google's export) reads every .ics in it.
func TestAZipOfCalendarFilesReadsEachOne(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"Takeout/Calendar/Sam.ics":      googleFile("jonas@example.com"),
		"Takeout/Calendar/Holidays.ics": "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:h\r\nSUMMARY:Holiday\r\nDTSTART;VALUE=DATE:20261225\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"Takeout/archive_browser.html":  "<html></html>",
		"Takeout/Calendar/broken.ics":   "not a calendar",
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	data := buf.Bytes()
	cals, skipped, err := readCalendarFiles(contentIOBusiness.Input{Filename: "takeout.zip", Size: int64(len(data)), File: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range cals {
		n += len(c.Events)
	}
	if len(cals) != 2 || n != 4 || len(skipped) != 1 || skipped[0].What != "Takeout/Calendar/broken.ics" {
		t.Fatalf("calendars %d, events %d, skipped %+v", len(cals), n, skipped)
	}
	if !(icsImporter{}).Accepts("takeout.zip", "") || !(icsImporter{}).Accepts("x", "text/calendar; charset=utf-8") || (icsImporter{}).Accepts("notes.md", "") {
		t.Fatal("the importer takes the wrong files")
	}
}

// Each calendar of an export comes in as itself: the one Google names by your
// address into your calendar, each other one into a calendar of its name,
// found again by a later import; a Meet link and files come in the notes,
// where Join finds the link.
func TestEachCalendarOfAnExportComesInAsItself(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	h.store.addresses["sam@work.example"] = personOf(sam)
	own := googleFile("jonas@example.com")
	offsites := strings.Join([]string{"BEGIN:VCALENDAR", "X-WR-CALNAME:Team offsites",
		"BEGIN:VEVENT", "UID:offsite", "SUMMARY:Offsite", "DTSTART:20261105T090000Z", "DTEND:20261105T170000Z",
		"X-GOOGLE-CONFERENCE:https://meet.google.com/abc-defg-hij", "DESCRIPTION:Bring a laptop",
		"ATTACH;FILENAME=Agenda:https://docs.google.com/document/d/1y", "END:VEVENT", "END:VCALENDAR", ""}, "\r\n")
	cals := append(readFile(t, own), readFile(t, offsites)...)
	made, err := h.s.importCalendars(context.Background(), sam, cals, func(int, int) {})
	if err != nil || len(made.Created) != 3 {
		t.Fatalf("made %+v %v", made, err)
	}
	personal := h.store.calendars[personOf(sam)]
	offsite := h.store.events[uuid.MustParse(made.Created[2].UUID)]
	if h.store.events[uuid.MustParse(made.Created[0].UUID)].CalendarID != personal || offsite.CalendarID == personal || offsite.CalendarID != h.store.imported[personOf(sam).String()+"/Team offsites"] {
		t.Fatalf("calendars: personal %s, offsite in %s", personal, offsite.CalendarID)
	}
	// Where they went, as the import's dialog lists them; and what Show them
	// opens: the weekly review goes on from now, so this week.
	offsitesCal := h.store.imported[personOf(sam).String()+"/Team offsites"]
	if made.Count != 3 || len(made.Into) != 2 || made.Into[0] != (contentIOBusiness.Into{UUID: personal.String(), Kind: "personal", Title: "Personal", Count: 2}) ||
		made.Into[1] != (contentIOBusiness.Into{UUID: offsitesCal.String(), Kind: "external", Title: "Team offsites", Count: 1}) {
		t.Fatalf("where they went: %d %+v", made.Count, made.Into)
	}
	if made.Show == nil || made.Show.UUID != made.Created[0].UUID || made.Show.At != h.clock.UTC().Format(time.RFC3339) {
		t.Fatalf("show %+v (clock %s)", made.Show, h.clock)
	}
	if offsite.Notes != "Join: https://meet.google.com/abc-defg-hij\n\nBring a laptop\n\nAttachments:\n- Agenda: https://docs.google.com/document/d/1y" {
		t.Fatalf("notes %q", offsite.Notes)
	}
	if got := callLinkIn(notesAsText(offsite)); got != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("call link %q", got)
	}
	again, _ := h.s.importCalendars(context.Background(), sam, cals, func(int, int) {})
	if len(again.Created) != 0 || len(again.Skipped) != 0 || again.Unchanged != 3 {
		t.Fatalf("again %+v", again)
	}
}

// A big import names its first events and counts the rest, so its job stays
// small; Show them opens the next event to come, else the latest.
func TestABigImportNamesItsFirstAndShowsTheNextToCome(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	lines := []string{"BEGIN:VCALENDAR", "X-WR-CALNAME:History"}
	// 150 single events, one a day from 120 days before the clock (16 Oct 2026).
	first := h.clock.AddDate(0, 0, -120).Truncate(24 * time.Hour).Add(9 * time.Hour)
	for i := 0; i < 150; i++ {
		at := first.AddDate(0, 0, i)
		lines = append(lines, "BEGIN:VEVENT", fmt.Sprintf("UID:day-%d", i), fmt.Sprintf("SUMMARY:Day %d", i),
			"DTSTART:"+at.Format("20060102T150405Z"), "DTEND:"+at.Add(time.Hour).Format("20060102T150405Z"), "END:VEVENT")
	}
	lines = append(lines, "END:VCALENDAR", "")
	made, err := h.s.importCalendars(context.Background(), sam, readFile(t, strings.Join(lines, "\r\n")), func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	// Every event made is there for the move from Google's sync refs; the
	// job names the first hundred.
	if made.Count != 150 || len(made.Created) != 150 || len(made.Into) != 1 || made.Into[0].Count != 150 || made.Into[0].Title != "History" {
		t.Fatalf("count %d, made %d, into %+v", made.Count, len(made.Created), made.Into)
	}
	if job := forJob(made); len(job.Created) != maxNamedImports || job.Count != 150 || job.Created[0].Title != "Day 0" {
		t.Fatalf("the job names %d of %d", len(job.Created), job.Count)
	}
	// Day 120 is the first at or after 08:00 on the clock's day (09:00 that day).
	next := first.AddDate(0, 0, 120)
	if made.Show == nil || made.Show.Title != "Day 120" || made.Show.At != next.Format(time.RFC3339) {
		t.Fatalf("show %+v, want Day 120 at %s", made.Show, next)
	}

	// Nothing to come: the latest.
	past := strings.Join([]string{"BEGIN:VCALENDAR", "X-WR-CALNAME:Old",
		"BEGIN:VEVENT", "UID:a", "SUMMARY:Older", "DTSTART:20250101T090000Z", "DTEND:20250101T100000Z", "END:VEVENT",
		"BEGIN:VEVENT", "UID:b", "SUMMARY:Newer", "DTSTART;VALUE=DATE:20250301", "END:VEVENT",
		"END:VCALENDAR", ""}, "\r\n")
	made, err = h.s.importCalendars(context.Background(), sam, readFile(t, past), func(int, int) {})
	if err != nil || made.Show == nil || made.Show.Title != "Newer" || made.Show.At != "2025-03-01" {
		t.Fatalf("show %+v %v", made.Show, err)
	}
}
