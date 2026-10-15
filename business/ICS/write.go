// Package ICS writes iCalendar (RFC 5545) for email invites (iMIP, RFC 6047).
// It grows into the calendar's one codec (A4): import, export and feeds read
// and write through it. Today it writes one event, with CRLF line ends, lines
// folded at 75 octets between characters, TEXT escaped and parameters quoted,
// so nothing a person types can change the calendar's structure.
package ICS

import (
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The iMIP methods.
const (
	Request = "REQUEST"
	Cancel  = "CANCEL"
)

// Address is someone on an event: their name, and the address mail reaches.
type Address struct {
	Name  string
	Email string
}

// Event is one event in an invite. UID names it across every message about
// it, and Sequence only grows, so a later message updates the event a
// calendar already has.
type Event struct {
	UID         string
	Sequence    int
	Stamp       time.Time
	Start, End  time.Time
	Summary     string
	Description string
	Organizer   Address
	Attendee    Address
}

// Write is an iCalendar message about ev: METHOD method, and the event
// CONFIRMED, or CANCELLED for a Cancel. Pure.
func Write(method string, ev Event) []byte {
	var b strings.Builder
	line := func(s string) {
		b.WriteString(fold(s))
		b.WriteString("\r\n")
	}
	status := "CONFIRMED"
	if method == Cancel {
		status = "CANCELLED"
	}
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:-//OneCamp//Calendar//EN")
	line("CALSCALE:GREGORIAN")
	line("METHOD:" + value(method))
	line("BEGIN:VEVENT")
	line("UID:" + value(ev.UID))
	line("SEQUENCE:" + strconv.Itoa(ev.Sequence))
	line("DTSTAMP:" + utc(ev.Stamp))
	line("DTSTART:" + utc(ev.Start))
	line("DTEND:" + utc(ev.End))
	line("SUMMARY:" + text(ev.Summary))
	if ev.Description != "" {
		line("DESCRIPTION:" + text(ev.Description))
	}
	line("ORGANIZER;CN=" + param(ev.Organizer.Name) + ":" + calAddress(ev.Organizer.Email))
	line("ATTENDEE;CN=" + param(ev.Attendee.Name) + ";ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=FALSE:" + calAddress(ev.Attendee.Email))
	line("STATUS:" + status)
	line("TRANSP:OPAQUE")
	line("END:VEVENT")
	line("END:VCALENDAR")
	return []byte(b.String())
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
