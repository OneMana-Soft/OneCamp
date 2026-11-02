package helpers

import (
	"fmt"
	"strings"
	"sync"
	"time"
	// The zone database is built in, so a zone's other names (Asia/Calcutta,
	// US/Eastern) load on any host: some keep them in a package of their own.
	_ "time/tzdata"
)

// Location is the IANA time zone a request named ("Asia/Kolkata"), or UTC
// when it named none or one Go doesn't know. Dates people pick (a due date)
// are their local midnight, so "which day" must be asked in their zone.
func Location(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

// FirstLocation is the first of names that is an IANA time zone Go knows,
// and whether there was one: a caller with a better fallback than UTC (a
// fixed offset, "time unknown") needs to know. Blank names and "Local", the
// server's own zone and nobody's, are passed over.
func FirstLocation(names ...string) (*time.Location, bool) {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || name == "Local" {
			continue
		}
		if loc, err := time.LoadLocation(name); err == nil {
			return loc, true
		}
	}
	return time.UTC, false
}

// SameZone is whether two zone names name one zone, never by spelling
// (V-135): Asia/Kolkata and Asia/Calcutta, Europe/Kyiv and Europe/Kiev, UTC
// and Etc/UTC. Equal names are one zone, a blank one too: nothing changed.
// Otherwise a blank name, "Local" or one Go doesn't know is no zone. Go keeps
// a zone's name as given, so two names are one zone when their records have
// agreed since 1970, change for change through 2037: the same offset under the
// same abbreviation, as a second name linked to one record does. London and
// Dublin, whose clocks have agreed since 1970, stay two: British Summer Time
// isn't Irish Standard Time. The web asks its runtime (lib/utils/sameZone.ts).
func SameZone(a, b string) bool {
	if a == b {
		return true
	}
	ra, ok := zoneRecord(a)
	if !ok {
		return false
	}
	rb, ok := zoneRecord(b)
	return ok && ra == rb
}

// zoneRecords keeps zoneRecord's answer for each name Go knows, and only
// those: at most the zone database's few hundred names, whatever names
// requests bring.
var zoneRecords sync.Map

// zoneRecord is a zone's offset and abbreviation from 1970, and each change
// of either through 2037 at its instant, read from Go's own zone data; false
// for a blank name, "Local" or one Go doesn't know.
func zoneRecord(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return "", false
	}
	if r, ok := zoneRecords.Load(name); ok {
		return r.(string), true
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return "", false
	}
	at, until := time.Unix(0, 0).In(loc), time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	abbr, off := at.Zone()
	var b strings.Builder
	fmt.Fprintf(&b, "%s%+d", abbr, off)
	for {
		_, next := at.ZoneBounds()
		if next.IsZero() || !next.After(at) || !next.Before(until) {
			break
		}
		at = next
		if a, o := at.Zone(); a != abbr || o != off {
			abbr, off = a, o
			fmt.Fprintf(&b, " %d:%s%+d", at.Unix(), abbr, off)
		}
	}
	r, _ := zoneRecords.LoadOrStore(name, b.String())
	return r.(string), true
}

// DayStart is the midnight that starts t's day in loc. Days are counted in
// the reader's zone, so "today" is the day their calendar shows.
func DayStart(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// PlaceName is a time zone as people say it: its city, "New York" for
// America/New_York, "Kolkata" for Asia/Kolkata, and UTC for UTC or none. For
// saying whose day a date is ("13 October, New York time").
func PlaceName(loc *time.Location) string {
	if loc == nil {
		return "UTC"
	}
	name := loc.String()
	if name == "" || name == "Local" || name == "UTC" {
		return "UTC"
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return strings.ReplaceAll(name, "_", " ")
}
