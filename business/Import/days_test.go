package business

import (
	"context"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A date a source keeps with no time (Asana's due_on, Jira's duedate,
// Linear's dueDate) was written as midnight UTC, and the web reads a due date
// in the viewer's own zone, so a team in Los Angeles saw every imported day
// as the day before (21 Oct 2026). It is now written as the day's
// midnight where the team is, or noon UTC when that isn't known.
func TestADayWithNoTimeLandsOnItsDayWhereTheTeamIs(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	day := func(s string) importProvider.Day {
		d, ok := importProvider.ParseDay(s)
		if !ok {
			t.Fatalf("ParseDay(%q)", s)
		}
		return d
	}
	at := time.Date(2026, 10, 22, 17, 30, 0, 0, time.UTC)

	cases := []struct {
		name       string
		st         importProvider.SourceTask
		loc        *time.Location
		start, due string // RFC 3339 in UTC; "" is none
	}{
		{
			name: "a due day, the team in Los Angeles: that day's midnight there",
			st:   importProvider.SourceTask{DueDay: day("2026-10-22")},
			loc:  la,
			due:  "2026-10-22T07:00:00Z",
		},
		{
			name: "a due day, the team in Kolkata: that day's midnight there",
			st:   importProvider.SourceTask{DueDay: day("2026-10-22")},
			loc:  kolkata,
			due:  "2026-10-21T18:30:00Z",
		},
		{
			name: "a due day with no zone known: noon UTC",
			st:   importProvider.SourceTask{DueDay: day("2026-10-22")},
			due:  "2026-10-22T12:00:00Z",
		},
		{
			name:  "a start day and a due time: the time stays as the source gave it",
			st:    importProvider.SourceTask{StartDay: day("2026-10-20"), DueDate: &at},
			loc:   la,
			start: "2026-10-20T07:00:00Z",
			due:   "2026-10-22T17:30:00Z",
		},
		{
			name: "no dates stay none",
			st:   importProvider.SourceTask{},
			loc:  la,
		},
	}
	show := func(v *time.Time) string {
		if v == nil {
			return ""
		}
		return v.UTC().Format(time.RFC3339)
	}
	for _, c := range cases {
		start, due := taskDates(&c.st, c.loc)
		if show(start) != c.start || show(due) != c.due {
			t.Errorf("%s: start %q due %q, want start %q due %q", c.name, show(start), show(due), c.start, c.due)
		}
	}

	// What it's for: whoever reads it, in Los Angeles or in Kolkata, sees the
	// source's day.
	for _, written := range []*time.Location{la, nil} {
		_, due := taskDates(&importProvider.SourceTask{DueDay: day("2026-10-22")}, written)
		for _, reader := range []*time.Location{la, kolkata} {
			got := "none"
			if due != nil {
				got = due.In(reader).Format("2006-01-02")
			}
			if got != "2026-10-22" {
				t.Errorf("written for %v, read in %v: %s, want 2026-10-22", written, reader, got)
			}
		}
	}
}

// The zone an import's days fall in is the one the admin's browser sent with
// the job; a name Go can't read is no zone, never a failed import, and so is
// "Local", the server's own zone and nobody's.
func TestTheImportsZoneIsTheOneItWasStartedIn(t *testing.T) {
	ctx := context.Background()
	if loc := dayZone(ctx, nil, importProvider.JobOptions{"time_zone": "America/Los_Angeles"}); loc == nil || loc.String() != "America/Los_Angeles" {
		t.Errorf("time_zone America/Los_Angeles: got %v", loc)
	}
	for _, opts := range []importProvider.JobOptions{nil, {}, {"time_zone": ""}, {"time_zone": "Local"}, {"time_zone": "Mars/Olympus"}, {"time_zone": 3}} {
		if loc := dayZone(ctx, nil, opts); loc != nil {
			t.Errorf("options %v: got %v, want none", opts, loc)
		}
	}
}
