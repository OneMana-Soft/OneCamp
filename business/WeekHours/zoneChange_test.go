package business

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain reads zones from Go's own copy of the zone database
// ($GOROOT/lib/time/zoneinfo.zip, through ZONEINFO, which Go reads before
// the host's): the same on every host. Its "slim" files leave a zone's later
// years to the zone's rule, where Go gives a leap year's last stretch an end
// on 31 December.
func TestMain(m *testing.M) {
	if root, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		os.Setenv("ZONEINFO", filepath.Join(strings.TrimSpace(string(root)), "lib", "time", "zoneinfo.zip"))
	}
	os.Exit(m.Run())
}

// nextOpenWithin is NextOpen, failed rather than waited on when it hasn't
// answered in 5 seconds (it never would: a loop that doesn't move on).
func nextOpenWithin(t *testing.T, h Hours, from time.Time, within time.Duration) (time.Time, bool) {
	t.Helper()
	type answer struct {
		at time.Time
		ok bool
	}
	done := make(chan answer, 1)
	go func() {
		at, ok := h.NextOpen(from, within)
		done <- answer{at, ok}
	}()
	select {
	case a := <-done:
		return a.at, a.ok
	case <-time.After(5 * time.Second):
		t.Fatalf("NextOpen from %v hasn't answered in 5 s", from)
		return time.Time{}, false
	}
}

// NextOpen moves on past a leap year's last day. Go's ZoneBounds gives the
// year's last stretch an end at 31 December 00:00 UTC, and from then on that
// end is the instant itself: NextOpen took it as the next change and never
// moved again. Hours are weekdays 1 (Monday) to 5 (Friday), as Hours reads
// Days (0 is Sunday).
func TestNextOpenPastALeapYearsEnd(t *testing.T) {
	weekdays := Hours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", TZ: "Europe/Kyiv"}
	kyiv, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatal(err)
	}
	// The year's end Go gives on these zone files (slim, or past 2037 on
	// any): without it this test proves nothing.
	if _, end := time.Date(2028, 12, 31, 0, 0, 0, 0, time.UTC).In(kyiv).ZoneBounds(); !end.Equal(time.Date(2028, 12, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Europe/Kyiv isn't read from Go's own slim copy here (2028's last stretch ends %v)", end)
	}
	for _, c := range []struct{ from, want time.Time }{
		// Saturday 30 December 2028 to Monday 1 January 2029, nine in Kyiv.
		{time.Date(2028, 12, 30, 12, 0, 0, 0, kyiv), time.Date(2029, 1, 1, 9, 0, 0, 0, kyiv)},
		// Sunday 30 December 2040, past 2037 where every zone file leaves
		// the years to the rule, to Monday 31 December.
		{time.Date(2040, 12, 30, 18, 0, 0, 0, time.UTC), time.Date(2040, 12, 31, 9, 0, 0, 0, kyiv)},
	} {
		if at, ok := nextOpenWithin(t, weekdays, c.from, 7*24*time.Hour); !ok || !at.Equal(c.want) {
			t.Errorf("from %v: got %v %v, want %v", c.from, at, ok, c.want)
		}
	}
	// Asked on the year's last day, after Go's end of the year: there
	// ZoneBounds' end is behind the instant, and NextOpen stepped back to it.
	if at, ok := nextOpenWithin(t, weekdays, time.Date(2040, 12, 31, 8, 0, 0, 0, kyiv), 3*time.Hour); !ok || !at.Equal(time.Date(2040, 12, 31, 9, 0, 0, 0, kyiv)) {
		t.Errorf("from 08:00 on 31 December 2040: got %v %v, want 09:00", at, ok)
	}
}
