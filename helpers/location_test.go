package helpers

import (
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
