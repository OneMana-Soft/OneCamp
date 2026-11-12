package helpers

import (
	"context"
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
// A name the zone database links to another (zoneLinks) is that one, with
// no zone data read at all: a host may keep those names in a package of
// their own (Ubuntu's tzdata-legacy), and Go then reads them from its own,
// older copy. Otherwise a blank name, "Local" or one Go doesn't know is no
// zone, and two names are one zone when their records have agreed since
// 1970, change for change through 2037: the same offset under the same
// abbreviation. London and Dublin, whose clocks have agreed since 1970, stay
// two: British Summer Time isn't Irish Standard Time. The web asks its
// runtime (lib/utils/sameZone.ts).
func SameZone(a, b string) bool {
	if a == b {
		return true
	}
	a, b = linkedZone(a), linkedZone(b)
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

// linkedZone is a zone name without the spaces around it, and the zone it
// names when it is one of the database's other names for a zone. Pure.
func linkedZone(name string) string {
	name = strings.TrimSpace(name)
	if zone, ok := zoneLinks[name]; ok {
		return zone
	}
	return name
}

// zoneRecords keeps zoneRecord's answer for each name Go knows, and only
// those: at most the zone database's few hundred names, whatever names
// requests bring.
var zoneRecords sync.Map

// zoneRecord is zoneRecordOf the zone of that name, as this host's zone data
// has it (Go's own copy when the host has none); false for a blank name,
// "Local" or one Go doesn't know.
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
	r, _ := zoneRecords.LoadOrStore(name, zoneRecordOf(loc))
	return r.(string), true
}

// zoneRecordOf is a zone's offset and abbreviation from 1970, and each change
// of either through 2037 at its instant. The same however the data spells
// it: a "fat" zone file lists each change through 2037, a "slim" one (Go's
// own copy) stops listing and leaves the later years to the zone's rule.
// Pure.
func zoneRecordOf(loc *time.Location) string {
	at, until := time.Unix(0, 0).In(loc), time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	abbr, off := at.Zone()
	var b strings.Builder
	fmt.Fprintf(&b, "%s%+d", abbr, off)
	for {
		next := NextZoneChange(at)
		if next.IsZero() || !next.Before(until) {
			break
		}
		at = next
		if a, o := at.Zone(); a != abbr || o != off {
			abbr, off = a, o
			fmt.Fprintf(&b, " %d:%s%+d", at.Unix(), abbr, off)
		}
	}
	return b.String()
}

// NextZoneChange is the next instant after t at which t's zone may change
// its offset or abbreviation: always after t, never t itself, or the zero
// Time when the zone never changes again. Between t and it the zone's clock
// runs evenly. Every walk over a zone's changes steps by it (zoneRecordOf,
// business/WeekHours NextOpen). Pure.
//
// It is Go's ZoneBounds end, but for one stretch. For the years a zone file
// leaves to the zone's rule (its "TZ footer": every year of a slim file
// after its last listed change, and of any file after 2037), Go (time's
// tzset) ends a year's last stretch 365 days after the year starts, counting
// the year in UTC. In a leap year that is 31 December 00:00 UTC, so from
// then until the year ends ZoneBounds' end is t itself or before it: a walk
// taking it stops there or never moves on. There the next change can come
// no sooner than the next year, whose rule Go works out afresh, so the
// answer is that year's start in UTC (in the zone's own time, a zone east of
// UTC starts its year before Go does, still in the old one).
func NextZoneChange(t time.Time) time.Time {
	_, end := t.ZoneBounds()
	if end.IsZero() || end.After(t) {
		return end
	}
	y := t.UTC().Year()
	return time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC).In(t.Location())
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

type zoneKey struct{}

// WithZone carries the time zone the request's reader is in (an IANA name,
// such as "Asia/Kolkata"), where a formula's TODAY() is and which day a
// date falls on. An empty or unknown zone leaves ctx as it is: ZoneOf is
// then UTC, as Airtable's formulas default to.
func WithZone(ctx context.Context, tz string) context.Context {
	if tz == "" {
		return ctx
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return ctx
	}
	return WithLocation(ctx, loc)
}

// WithLocation is WithZone for a zone already read; nil leaves ctx as it is.
func WithLocation(ctx context.Context, loc *time.Location) context.Context {
	if loc == nil {
		return ctx
	}
	return context.WithValue(ctx, zoneKey{}, loc)
}

// ZoneOf is the reader's time zone WithZone carries, else UTC.
func ZoneOf(ctx context.Context) *time.Location {
	if loc, ok := ctx.Value(zoneKey{}).(*time.Location); ok {
		return loc
	}
	return time.UTC
}

// PriorityLabel is a task's priority as people read it ("high" is High),
// none for none.
func PriorityLabel(p string) string {
	if p == "" {
		return ""
	}
	return strings.ToUpper(p[:1]) + p[1:]
}
