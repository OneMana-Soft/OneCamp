package helpers

import (
	"fmt"
	"testing"
	"time"
)

func TestLocation(t *testing.T) {
	if Location("Asia/Kolkata").String() != "Asia/Kolkata" {
		t.Error("a known zone")
	}
	for _, bad := range []string{"", "Local", "Mars/Olympus", "../../etc/passwd"} {
		if Location(bad) != time.UTC {
			t.Errorf("%q must fall back to UTC", bad)
		}
	}
}

// FirstLocation is the one "first of these names that is a zone": the
// assistant wrote it three times, each accepting "Local", the server's zone.
func TestFirstLocation(t *testing.T) {
	if loc, ok := FirstLocation("", " ", "Local", "Mars/Olympus", "Asia/Kolkata", "Europe/London"); !ok || loc.String() != "Asia/Kolkata" {
		t.Errorf("got %s %v, want the first known zone", loc, ok)
	}
	if loc, ok := FirstLocation("Local", "nope"); ok || loc != time.UTC {
		t.Errorf("got %s %v, want UTC and unknown", loc, ok)
	}
	if _, ok := FirstLocation(); ok {
		t.Error("no names is a zone")
	}
}

// SameZone is one zone by any of its names, never by spelling (V-135).
func TestSameZone(t *testing.T) {
	for _, p := range [][2]string{
		{"Asia/Kolkata", "Asia/Calcutta"}, {"Europe/Kyiv", "Europe/Kiev"}, {"UTC", "Etc/UTC"}, {"Etc/UTC", "Zulu"},
		{"America/New_York", "US/Eastern"}, {"America/Indiana/Indianapolis", "America/Indianapolis"},
		// Names a host may keep in another tzdata release than Go's own copy
		// (Canada/Pacific in tzdata-legacy, beside the main tzdata).
		{"America/Vancouver", "Canada/Pacific"},
		{" Asia/Kolkata ", "Asia/Calcutta"},
		// Equal names are one zone, whatever they name: nothing changed.
		{"Asia/Kolkata", "Asia/Kolkata"}, {"", ""}, {"Mars/Olympus", "Mars/Olympus"},
	} {
		if !SameZone(p[0], p[1]) || !SameZone(p[1], p[0]) {
			t.Errorf("%q and %q are one zone", p[0], p[1])
		}
	}
	// Other zones, and zones whose clocks agree under names of their own:
	// Dublin's summer is Irish Standard Time, London's British Summer Time;
	// Abidjan keeps GMT, and Etc/GMT is a zone apart from UTC.
	for _, p := range [][2]string{
		{"Asia/Kolkata", "Asia/Colombo"}, {"Europe/Berlin", "Europe/Paris"}, {"America/New_York", "America/Toronto"},
		{"Europe/London", "Europe/Dublin"}, {"UTC", "Africa/Abidjan"}, {"UTC", "Etc/GMT"},
		// A blank name, the server's own zone or one Go doesn't know is none.
		{"", "UTC"}, {"Local", "UTC"}, {"Mars/Olympus", "UTC"}, {"Mars/Olympus", "Mars/Base"}, {"../../etc/passwd", "UTC"},
	} {
		if SameZone(p[0], p[1]) || SameZone(p[1], p[0]) {
			t.Errorf("%q and %q are two zones", p[0], p[1])
		}
	}
}

// SameZone keeps a record for each name Go knows and for no other, so names
// that requests make up can't grow what it keeps.
func TestSameZoneKeepsOnlyZones(t *testing.T) {
	kept := func() (n int) {
		zoneRecords.Range(func(any, any) bool { n++; return true })
		return n
	}
	SameZone("Asia/Kolkata", "Asia/Calcutta")
	was := kept()
	for i := range 200 {
		SameZone(fmt.Sprintf("Nowhere/%d", i), "Asia/Kolkata")
	}
	if now := kept(); now != was {
		t.Errorf("kept %d records, then %d after 200 unknown names", was, now)
	}
}

func TestPlaceName(t *testing.T) {
	for zone, want := range map[string]string{"America/New_York": "New York", "Asia/Kolkata": "Kolkata", "UTC": "UTC", "": "UTC", "America/Argentina/Buenos_Aires": "Buenos Aires"} {
		if got := PlaceName(Location(zone)); got != want {
			t.Errorf("PlaceName(%q) = %q, want %q", zone, got, want)
		}
	}
	if got := PlaceName(nil); got != "UTC" {
		t.Errorf("PlaceName(nil) = %q", got)
	}
}
