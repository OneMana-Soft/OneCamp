// Package ICS writes iCalendar (RFC 5545) for email invitations (iMIP, RFC
// 6047, with iTIP's methods, RFC 5546). It grows into the calendar's one codec
// (A4): import, export and feeds read and write through it.
//
// It writes an event alone, or a repeating event as its series and the
// instances changed on their own (one VEVENT each, sharing a UID), with
// all-day dates, and a repeating event's times in its zone with that zone's
// VTIMEZONE (vtimezone.go), so 09:15 in London stays 09:15 through the
// clock change in every calendar. Lines end in CRLF and are folded at 75
// octets between characters, TEXT is escaped and parameters quoted, so
// nothing a person types can change the calendar's structure.
package ICS

import (
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The iTIP methods.
const (
	Request = "REQUEST"
	Cancel  = "CANCEL"
)

// The participation states an attendee answers with (PARTSTAT).
const (
	NeedsAction = "NEEDS-ACTION"
	Accepted    = "ACCEPTED"
	Declined    = "DECLINED"
	Tentative   = "TENTATIVE"
)

// Address is someone on an event: their name, and the address mail reaches.
type Address struct {
	Name  string
	Email string
}

// Attendee is a guest on an event, with their answer.
type Attendee struct {
	Address
	// Optional is a guest whose being there is optional (OPT-PARTICIPANT).
	Optional bool
	// PartStat is their answer (NeedsAction when they haven't given one).
	PartStat string
	// RSVP asks them for an answer.
	RSVP bool
}

// Event is one VEVENT. UID names it across every message about it, and
// Sequence only grows, so a later message updates the event a calendar
// already has.
type Event struct {
	UID      string
	Sequence int
	Stamp    time.Time
	// When: instants (Start, End), or for an all-day event dates
	// (StartDate, EndDate: "2026-10-16", the end the day after its last).
	Start, End         time.Time
	StartDate, EndDate string
	// Zone is the IANA zone a repeating event keeps its time in: its times
	// are then written in that zone, beside its VTIMEZONE. Empty, they are
	// written in UTC.
	Zone string
	// RRule repeats it (an RFC 5545 RRULE value), ExDates are the instances
	// it skips, and RecurrenceID, on a changed instance of a series, is the
	// start it had in the series. For an all-day series these are dates at
	// 00:00 UTC.
	RRule        string
	ExDates      []time.Time
	RecurrenceID *time.Time
	Summary      string
	Description  string
	Location     string
	// Status is CONFIRMED, TENTATIVE or CANCELLED; empty, it follows the
	// method (CANCELLED for a Cancel).
	Status      string
	Transparent bool
	Organizer   Address
	// Attendees are its guests. A booking's invite names its one guest, who
	// booked it, as Attendee instead: accepted, with no answer asked.
	Attendees []Attendee
	Attendee  Address
}

// Message is an iCalendar message: a method and the VEVENTs of one event.
type Message struct {
	Method string
	Events []Event
}

// Write is an iCalendar message about ev alone: METHOD method, and the event
// CONFIRMED, or CANCELLED for a Cancel. Pure.
func Write(method string, ev Event) []byte {
	return WriteMessage(Message{Method: method, Events: []Event{ev}})
}

// WriteMessage writes m: its time zones first, then its events. Pure.
func WriteMessage(m Message) []byte {
	var b strings.Builder
	line := func(s string) {
		b.WriteString(fold(s))
		b.WriteString("\r\n")
	}
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:-//OneCamp//Calendar//EN")
	line("CALSCALE:GREGORIAN")
	line("METHOD:" + value(m.Method))
	for _, z := range zonesOf(m.Events) {
		for _, l := range z {
			line(l)
		}
	}
	for _, ev := range m.Events {
		writeEvent(line, m.Method, ev)
	}
	line("END:VCALENDAR")
	return []byte(b.String())
}

func writeEvent(line func(string), method string, ev Event) {
	status := ev.Status
	if status == "" {
		status = "CONFIRMED"
		if method == Cancel {
			status = "CANCELLED"
		}
	}
	allDay := ev.StartDate != ""
	loc := zoneOf(ev)
	line("BEGIN:VEVENT")
	line("UID:" + value(ev.UID))
	line("SEQUENCE:" + strconv.Itoa(ev.Sequence))
	line("DTSTAMP:" + utc(ev.Stamp))
	if ev.RecurrenceID != nil {
		line("RECURRENCE-ID" + when(*ev.RecurrenceID, allDay, loc))
	}
	if allDay {
		line("DTSTART;VALUE=DATE:" + compactDate(ev.StartDate))
		line("DTEND;VALUE=DATE:" + compactDate(ev.EndDate))
	} else {
		line("DTSTART" + when(ev.Start, false, loc))
		line("DTEND" + when(ev.End, false, loc))
	}
	if ev.RRule != "" {
		line("RRULE:" + value(ev.RRule))
	}
	if len(ev.ExDates) > 0 {
		line("EXDATE" + whenList(ev.ExDates, allDay, loc))
	}
	line("SUMMARY:" + text(ev.Summary))
	if ev.Description != "" {
		line("DESCRIPTION:" + text(ev.Description))
	}
	if ev.Location != "" {
		line("LOCATION:" + text(ev.Location))
	}
	line("ORGANIZER;CN=" + param(ev.Organizer.Name) + ":" + calAddress(ev.Organizer.Email))
	if len(ev.Attendees) == 0 && ev.Attendee.Email != "" {
		line("ATTENDEE;CN=" + param(ev.Attendee.Name) + ";ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=FALSE:" + calAddress(ev.Attendee.Email))
	}
	for _, a := range ev.Attendees {
		role, stat, rsvp := "REQ-PARTICIPANT", a.PartStat, "FALSE"
		if a.Optional {
			role = "OPT-PARTICIPANT"
		}
		if stat == "" {
			stat = NeedsAction
		}
		if a.RSVP {
			rsvp = "TRUE"
		}
		line("ATTENDEE;CN=" + param(a.Name) + ";ROLE=" + role + ";PARTSTAT=" + paramText(stat) + ";RSVP=" + rsvp + ":" + calAddress(a.Email))
	}
	line("STATUS:" + value(status))
	if ev.Transparent {
		line("TRANSP:TRANSPARENT")
	} else {
		line("TRANSP:OPAQUE")
	}
	line("END:VEVENT")
}

// zoneOf is where ev's times are written: its zone, when it names one Go
// knows and that isn't UTC; nil (UTC) otherwise. Pure.
func zoneOf(ev Event) *time.Location {
	if ev.Zone == "" || ev.Zone == "UTC" || ev.StartDate != "" {
		return nil
	}
	loc, err := time.LoadLocation(ev.Zone)
	if err != nil {
		return nil
	}
	return loc
}

// when is a DATE-TIME's parameters and value: ";TZID=Europe/London:20261016T091500"
// in a zone, ":20261016T081500Z" in UTC, ";VALUE=DATE:20261016" for a day
// (an all-day series' instants are its dates at 00:00 UTC). Pure.
func when(t time.Time, date bool, loc *time.Location) string {
	switch {
	case date:
		return ";VALUE=DATE:" + t.UTC().Format("20060102")
	case loc != nil:
		return ";TZID=" + paramText(loc.String()) + ":" + t.In(loc).Format("20060102T150405")
	}
	return ":" + utc(t)
}

// whenList is several times as one property's parameters and values, in
// order. Pure.
func whenList(ts []time.Time, date bool, loc *time.Location) string {
	sorted := append([]time.Time(nil), ts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	params, first, _ := strings.Cut(when(sorted[0], date, loc), ":")
	vals := []string{first}
	for _, t := range sorted[1:] {
		_, v, _ := strings.Cut(when(t, date, loc), ":")
		vals = append(vals, v)
	}
	return params + ":" + strings.Join(vals, ",")
}

// compactDate is "2026-10-16" as a DATE value, "20261016". Pure.
func compactDate(d string) string { return strings.ReplaceAll(d, "-", "") }

// zonesOf is a VTIMEZONE for each zone the events are written in, covering
// the years they reach: from their first start to a series' last one, or ten
// years on for a series with no end. Pure.
func zonesOf(evs []Event) [][]string {
	type reach struct {
		loc      *time.Location
		from, to time.Time
	}
	byName := map[string]*reach{}
	var names []string
	for _, ev := range evs {
		loc := zoneOf(ev)
		if loc == nil {
			continue
		}
		last := ev.End
		if ev.RRule != "" {
			last = untilOf(ev.RRule, ev.Start.AddDate(10, 0, 0))
		}
		r := byName[loc.String()]
		if r == nil {
			r = &reach{loc: loc, from: ev.Start, to: last}
			byName[loc.String()] = r
			names = append(names, loc.String())
		}
		if ev.Start.Before(r.from) {
			r.from = ev.Start
		}
		if last.After(r.to) {
			r.to = last
		}
	}
	sort.Strings(names)
	out := make([][]string, 0, len(names))
	for _, n := range names {
		r := byName[n]
		out = append(out, vtimezone(r.loc, r.from, r.to))
	}
	return out
}

// untilOf is a rule's UNTIL as an instant, or otherwise when it has none.
// Pure.
func untilOf(rule string, otherwise time.Time) time.Time {
	for _, part := range strings.Split(rule, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(k, "UNTIL") {
			continue
		}
		for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t
			}
		}
	}
	return otherwise
}

// calAddress is a cal-address (RFC 5545 3.3.3): mailto: and the address
// alone. A sender set as "Acme <hi@acme.com>", which mail accepts, is
// hi@acme.com here. Pure.
func calAddress(email string) string {
	if a, err := mail.ParseAddress(email); err == nil {
		email = a.Address
	}
	return "mailto:" + value(email)
}

func utc(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

var escaper = strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`)

// text escapes a TEXT value (RFC 5545 3.3.11). Pure.
func text(s string) string { return escaper.Replace(s) }

// value drops what can't be in a value: control characters, which could end
// the line. Pure.
func value(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// param quotes a parameter value; a double quote can't be in one (RFC 5545
// 3.1), so it goes, with control characters. Pure.
func param(s string) string {
	return `"` + strings.ReplaceAll(value(s), `"`, "") + `"`
}

// paramText is a parameter value unquoted, as a zone's name is written: what
// can't be in one unquoted (";", ":", ",", a quote, control characters) goes.
// Pure.
func paramText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == ';' || r == ':' || r == ',' || r == '"' {
			return -1
		}
		return r
	}, s)
}

// fold folds a content line longer than 75 octets (RFC 5545 3.1), never
// inside a character: each continuation starts with a space. Pure.
func fold(s string) string {
	if len(s) <= 75 {
		return s
	}
	var b strings.Builder
	limit := 75
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		b.WriteString(s[:cut])
		b.WriteString("\r\n ")
		s = s[cut:]
		limit = 74 // the leading space counts
	}
	b.WriteString(s)
	return b.String()
}
