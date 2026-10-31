package provider

import (
	"testing"
	"time"
)

// A source's date-only value is a calendar day, and it is read back as that
// same day by people west and east of UTC.
func TestADayIsACalendarDateNeverMidnightUTC(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}

	d, ok := ParseDay("2026-10-22")
	if !ok || d != (Day{2026, time.October, 22}) || d.String() != "2026-10-22" {
		t.Fatalf("ParseDay(2026-10-22) = %+v, %v", d, ok)
	}
	for _, s := range []string{"", "2026-10-22T17:00:00", "2026-10-22T17:00:00Z", "22/10/2026", "2026-13-01"} {
		if d, ok := ParseDay(s); ok || !d.IsZero() {
			t.Errorf("ParseDay(%q) = %+v, %v; want no day", s, d, ok)
		}
	}

	// Written for a team in Los Angeles, it is that day's midnight there.
	if got := d.At(la).UTC().Format(time.RFC3339); got != "2026-10-22T07:00:00Z" {
		t.Errorf("At(Los Angeles) = %s, want 2026-10-22T07:00:00Z", got)
	}
	// With no zone known it is noon UTC.
	if got := d.At(nil).UTC().Format(time.RFC3339); got != "2026-10-22T12:00:00Z" {
		t.Errorf("At(nil) = %s, want 2026-10-22T12:00:00Z", got)
	}
	// Either way, Los Angeles and Kolkata both read the source's day.
	for _, written := range []*time.Location{la, nil} {
		for _, reader := range []*time.Location{la, kolkata} {
			if got := d.At(written).In(reader).Format("2006-01-02"); got != "2026-10-22" {
				t.Errorf("written for %v, read in %v: %s, want 2026-10-22", written, reader, got)
			}
		}
	}
	if !(Day{}).IsZero() || (Day{}).String() != "" {
		t.Error("the zero Day is no day")
	}
}
