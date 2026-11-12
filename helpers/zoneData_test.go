package helpers

import (
	"archive/zip"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// goZoneFiles is Go's own copy of the zone database
// ($GOROOT/lib/time/zoneinfo.zip), each name's file: "slim" files, which
// stop listing a zone's changes after its last change of rule and leave the
// later years to the rule. The same on every host, whatever zone data the
// host keeps, so these tests are hermetic.
var goZoneFiles = sync.OnceValues(func() (map[string][]byte, error) {
	root, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, err
	}
	r, err := zip.OpenReader(filepath.Join(strings.TrimSpace(string(root)), "lib", "time", "zoneinfo.zip"))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	files := map[string][]byte{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		files[f.Name] = b
	}
	return files, nil
})

// goZone is the zone of that name as Go's own copy of the zone database has it.
func goZone(t *testing.T, name string) *time.Location {
	t.Helper()
	files, err := goZoneFiles()
	if err != nil {
		t.Fatalf("Go's zone data: %v", err)
	}
	data, ok := files[name]
	if !ok {
		t.Fatalf("%s isn't in Go's zone data", name)
	}
	loc, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return loc
}

// lastChange is the year of a zone record's last change, 0 for none.
func lastChange(t *testing.T, record string) int {
	t.Helper()
	fields := strings.Fields(record)
	if len(fields) < 2 {
		return 0
	}
	at, _, _ := strings.Cut(fields[len(fields)-1], ":")
	sec, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		t.Fatalf("record %q: %v", record, err)
	}
	return time.Unix(sec, 0).UTC().Year()
}

// A zone's record runs to 2037 on slim data too. Go ends a ruled year's last
// stretch 365 days after the year starts, 31 December in a leap year, and a
// walk that took that end as a change stopped there: Kyiv's record ended in
// 1996 and New York's in 2008, so a host's full copy and Go's slim one gave
// one zone two records (the v2.79.0 CI: Europe/Kiev from Go's copy, Europe/Kyiv
// from the host's).
func TestZoneRecordRunsTo2037OnSlimData(t *testing.T) {
	for _, name := range []string{"Europe/Kyiv", "America/New_York", "Europe/London", "Australia/Sydney"} {
		record := zoneRecordOf(goZone(t, name))
		if got := lastChange(t, record); got != 2037 {
			t.Errorf("%s: the record's last change is in %d, want 2037 (%s)", name, got, record)
		}
	}
	// A zone that stopped changing its clock keeps its last change.
	if got := lastChange(t, zoneRecordOf(goZone(t, "Asia/Kolkata"))); got != 0 {
		t.Errorf("Asia/Kolkata has changed its clock since 1970, in %d", got)
	}
}

// Santiago and Coyhaique split in 2025 (Aysén keeps summer time all year):
// two zones. Records cut short in a leap year's last days agreed.
func TestSantiagoAndCoyhaiqueAreTwoZones(t *testing.T) {
	santiago, coyhaique := zoneRecordOf(goZone(t, "America/Santiago")), zoneRecordOf(goZone(t, "America/Coyhaique"))
	if santiago == coyhaique {
		t.Errorf("America/Santiago and America/Coyhaique have one record: %s", santiago)
	}
}

// NextZoneChange always moves on: past the end of a leap year that Go's
// ZoneBounds gives as the instant itself, to the change it hides.
func TestNextZoneChangeAlwaysMovesOn(t *testing.T) {
	for _, c := range []struct {
		zone string
		from time.Time
		want time.Time // the next real change of clock
	}{
		{"Europe/Kyiv", time.Date(2028, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(2029, 3, 25, 1, 0, 0, 0, time.UTC)},
		{"Europe/Kyiv", time.Date(2028, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2029, 3, 25, 1, 0, 0, 0, time.UTC)},
		{"America/New_York", time.Date(2028, 12, 31, 12, 0, 0, 0, time.UTC), time.Date(2029, 3, 11, 7, 0, 0, 0, time.UTC)},
		// Past 2037, where every zone file leaves the years to the rule.
		{"Europe/Kyiv", time.Date(2040, 12, 30, 18, 0, 0, 0, time.UTC), time.Date(2041, 3, 31, 1, 0, 0, 0, time.UTC)},
		{"Australia/Sydney", time.Date(2040, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(2041, 4, 6, 16, 0, 0, 0, time.UTC)},
	} {
		loc := goZone(t, c.zone)
		at := c.from.In(loc)
		_, offset := at.Zone()
		for steps := 0; ; steps++ {
			next := NextZoneChange(at)
			if !next.After(at) {
				t.Fatalf("%s from %v: the next change is %v, not after it", c.zone, c.from, next)
			}
			if steps > 4 || next.After(c.want) {
				t.Fatalf("%s from %v: walked past %v to %v", c.zone, c.from, c.want, next)
			}
			at = next
			if _, o := at.Zone(); o != offset {
				if !at.Equal(c.want) {
					t.Errorf("%s from %v: the clock changed at %v, want %v", c.zone, c.from, at, c.want)
				}
				break
			}
		}
	}
	// A zone that never changes again has no next change.
	if next := NextZoneChange(time.Date(2028, 12, 31, 0, 0, 0, 0, goZone(t, "Asia/Kolkata"))); !next.IsZero() {
		t.Errorf("Asia/Kolkata changes its clock at %v", next)
	}
	if next := NextZoneChange(time.Date(2028, 12, 31, 0, 0, 0, 0, time.UTC)); !next.IsZero() {
		t.Errorf("UTC changes its clock at %v", next)
	}
}

// Every zone's walk from 1970 to 2038 moves on at each step and ends.
func TestEveryZoneWalksTo2038(t *testing.T) {
	files, err := goZoneFiles()
	if err != nil {
		t.Fatalf("Go's zone data: %v", err)
	}
	until := time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	for name := range files {
		at := time.Unix(0, 0).In(goZone(t, name))
		for steps := 0; ; steps++ {
			next := NextZoneChange(at)
			if next.IsZero() || !next.Before(until) {
				break
			}
			if !next.After(at) || steps > 1000 {
				t.Errorf("%s: stuck at %v (step %d, next %v)", name, at, steps, next)
				break
			}
			at = next
		}
	}
}

// The link table is the zone database's own: each other name for a zone has
// that zone's record in Go's copy, and names a zone, not another link.
func TestZoneLinksAgreeWithGoData(t *testing.T) {
	for _, want := range [][2]string{
		{"Europe/Kiev", "Europe/Kyiv"}, {"US/Eastern", "America/New_York"}, {"Canada/Pacific", "America/Vancouver"},
		{"Asia/Calcutta", "Asia/Kolkata"}, {"UTC", "Etc/UTC"}, {"Zulu", "Etc/UTC"},
	} {
		if got := zoneLinks[want[0]]; got != want[1] {
			t.Errorf("%s links to %q, want %s", want[0], got, want[1])
		}
	}
	for link, zone := range zoneLinks {
		if _, ok := zoneLinks[zone]; ok {
			t.Errorf("%s links to %s, another link", link, zone)
		}
		if a, b := zoneRecordOf(goZone(t, link)), zoneRecordOf(goZone(t, zone)); a != b {
			t.Errorf("%s and %s have two records:\n%s\n%s", link, zone, a, b)
		}
	}
}

// A zone's other name is that zone with no zone data read: the host may not
// keep the name at all (Ubuntu's tzdata-legacy), or keep it in another
// release of the database than the zone's own.
func TestSameZoneReadsNoDataForALink(t *testing.T) {
	kept := func() (n int) {
		zoneRecords.Range(func(any, any) bool { n++; return true })
		return n
	}
	was := kept()
	for _, p := range [][2]string{{"US/Pacific", "America/Los_Angeles"}, {"Canada/Atlantic", "America/Halifax"}, {"Europe/Kiev", "Europe/Kyiv"}} {
		if !SameZone(p[0], p[1]) {
			t.Errorf("%s and %s are two zones", p[0], p[1])
		}
	}
	if now := kept(); now != was {
		t.Errorf("read %d zone records for a zone's other names", now-was)
	}
}
