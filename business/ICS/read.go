package ICS

// Reading iCalendar (calendar A4): the other half of the one codec. Import,
// feeds and sync read through Read, and write through WriteMessage, so a
// calendar read and written again keeps its series, the instances changed or
// skipped in it, its zones, guests, answers and alarms (G-ICS).
//
// ICS in the wild is messy. Google, Outlook and Apple each write it their own
// way: Outlook names zones by their Windows names ("W. Europe Standard
// Time"), Apple and Mozilla quote them or put a path in front, some writers
// fold lines with a tab or end them in LF alone, and many leave a timed event
// with no zone at all ("floating"). Read takes each of these, and what it
// can't take it leaves out with a reason in words, one event at a time: one
// bad event never loses the rest.

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Calendar is what one iCalendar file holds.
type Calendar struct {
	// Method is the iTIP method ("REQUEST", "CANCEL", "PUBLISH"), or "" for a
	// plain export.
	Method string
	// Name is the calendar's name as its writer gave it (X-WR-CALNAME).
	Name string
	// Zone is the zone its floating times are read in (X-WR-TIMEZONE), when
	// the writer named one Go knows.
	Zone   string
	Events []Event
	// Skipped is what was left out, and why, in words.
	Skipped []Skip
}

// Skip is one thing a read left out.
type Skip struct {
	// What names it: an event's title and start, or a component's kind.
	What string
	Why  string
}

// Alarm is a reminder on an event (VALARM), a time before its start.
type Alarm struct {
	Before time.Duration
	// Email is an alarm that sends an email (ACTION:EMAIL); otherwise it
	// shows on screen.
	Email bool
}

// The most a read takes: a line, and the events of one file.
const (
	maxLineBytes  = 1 << 20
	maxReadEvents = 50000
)

// Read reads an iCalendar file: its events, each series with the instances
// changed in it, in the order written. It fails only when the file isn't
// iCalendar at all; anything inside it that can't be read is in Skipped.
func Read(r io.Reader) (Calendar, error) {
	lines, err := unfoldLines(r)
	if err != nil {
		return Calendar{}, err
	}
	var cal Calendar
	var stack []string
	var cur []contentLine // the VEVENT being read
	var alarm []contentLine
	var other string                     // the title of a to-do or journal entry being skipped
	zones := map[string]*time.Location{} // a VTIMEZONE's TZID, read as a zone Go knows or a fixed offset
	var zoneLines []contentLine
	seen := false
	for _, raw := range lines {
		cl, ok := parseLine(raw)
		if !ok {
			continue
		}
		switch cl.name {
		case "BEGIN":
			kind := strings.ToUpper(cl.value)
			stack = append(stack, kind)
			switch kind {
			case "VCALENDAR":
				seen = true
			case "VEVENT":
				cur = cur[:0]
			case "VALARM":
				alarm = alarm[:0]
			case "VTIMEZONE":
				zoneLines = zoneLines[:0]
			}
			continue
		case "END":
			kind := strings.ToUpper(cl.value)
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
			switch kind {
			case "VEVENT":
				if len(cal.Events) >= maxReadEvents {
					cal.Skipped = append(cal.Skipped, Skip{What: "Events after the first " + strconv.Itoa(maxReadEvents), Why: "A file holds at most " + strconv.Itoa(maxReadEvents) + " events."})
					continue
				}
				ev, skip := readEvent(cur, &cal, zones)
				if skip != nil {
					cal.Skipped = append(cal.Skipped, *skip)
				} else {
					cal.Events = append(cal.Events, ev)
				}
				cur = cur[:0]
			case "VALARM":
				if len(stack) > 0 && stack[len(stack)-1] == "VEVENT" {
					cur = append(cur, contentLine{name: "X-ONECAMP-ALARM", alarm: append([]contentLine(nil), alarm...)})
				}
			case "VTIMEZONE":
				if id, loc := readZone(zoneLines); id != "" && loc != nil {
					zones[id] = loc
				}
			case "VTODO", "VJOURNAL":
				// Named by its title, as a person knows it.
				what, why := "A to-do", "Only events come across, not to-dos."
				if kind == "VJOURNAL" {
					what, why = "A journal entry", "Only events come across, not journal entries."
				}
				if other != "" {
					what = other
				}
				cal.Skipped = append(cal.Skipped, Skip{What: what, Why: why})
				other = ""
			}
			continue
		}
		if len(stack) == 0 {
			continue
		}
		switch stack[len(stack)-1] {
		case "VCALENDAR":
			switch cl.name {
			case "METHOD":
				cal.Method = strings.ToUpper(cl.value)
			case "X-WR-CALNAME":
				cal.Name = unescape(cl.value)
			case "X-WR-TIMEZONE":
				if loc := zoneNamed(cl.value); loc != nil {
					cal.Zone = loc.String()
				}
			}
		case "VEVENT":
			cur = append(cur, cl)
		case "VTODO", "VJOURNAL":
			if cl.name == "SUMMARY" {
				other = unescape(cl.value)
			}
		case "VALARM":
			alarm = append(alarm, cl)
		case "VTIMEZONE", "STANDARD", "DAYLIGHT":
			cl.section = stack[len(stack)-1]
			zoneLines = append(zoneLines, cl)
		}
	}
	if !seen {
		return Calendar{}, fmt.Errorf("This isn't a calendar file: it has no BEGIN:VCALENDAR.")
	}
	return cal, nil
}

// contentLine is one content line: NAME;PARAM=V:value.
type contentLine struct {
	name    string
	params  map[string]string
	value   string
	section string        // in a VTIMEZONE: which part it was in
	alarm   []contentLine // X-ONECAMP-ALARM: the VALARM's own lines
}

// unfoldLines joins folded lines (a CRLF or LF followed by a space or a tab).
func unfoldLines(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	var out []string
	first := true
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if first {
			l = strings.TrimPrefix(l, "\ufeff")
			first = false
		}
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(out) > 0 {
			out[len(out)-1] += l[1:]
			continue
		}
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("The calendar file couldn't be read: %w", err)
	}
	return out, nil
}

// parseLine splits a content line, honouring quoted parameter values, which
// may hold ";", ":" and ",".
func parseLine(s string) (contentLine, bool) {
	var cl contentLine
	i, quoted := 0, false
	nameEnd, colon := -1, -1
	for ; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
		if c == ';' && nameEnd < 0 {
			nameEnd = i
		}
		if c == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return cl, false
	}
	if nameEnd < 0 {
		nameEnd = colon
	}
	cl.name = strings.ToUpper(strings.TrimSpace(s[:nameEnd]))
	cl.value = s[colon+1:]
	if nameEnd < colon {
		cl.params = map[string]string{}
		for _, p := range splitParams(s[nameEnd+1 : colon]) {
			k, v, _ := strings.Cut(p, "=")
			cl.params[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(v, `"`)
		}
	}
	return cl, cl.name != ""
}

func splitParams(s string) []string {
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case ';':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

var unescaper = strings.NewReplacer(`\\`, `\`, `\;`, ";", `\,`, ",", `\n`, "\n", `\N`, "\n")

// unescape reads a TEXT value (RFC 5545 3.3.11).
func unescape(s string) string { return unescaper.Replace(s) }

// readEvent reads one VEVENT, or says why it was left out.
func readEvent(lines []contentLine, cal *Calendar, zones map[string]*time.Location) (Event, *Skip) {
	var ev Event
	var start, end *contentLine
	var duration string
	for i := range lines {
		cl := &lines[i]
		switch cl.name {
		case "UID":
			ev.UID = strings.TrimSpace(cl.value)
		case "SEQUENCE":
			ev.Sequence, _ = strconv.Atoi(strings.TrimSpace(cl.value))
		case "DTSTAMP":
			ev.Stamp, _ = time.Parse("20060102T150405Z", strings.TrimSpace(cl.value))
		case "DTSTART":
			start = cl
		case "DTEND":
			end = cl
		case "DURATION":
			duration = strings.TrimSpace(cl.value)
		case "SUMMARY":
			ev.Summary = unescape(cl.value)
		case "DESCRIPTION":
			ev.Description = unescape(cl.value)
		case "LOCATION":
			ev.Location = unescape(cl.value)
		case "STATUS":
			ev.Status = strings.ToUpper(strings.TrimSpace(cl.value))
		case "TRANSP":
			ev.Transparent = strings.EqualFold(strings.TrimSpace(cl.value), "TRANSPARENT")
		case "RRULE":
			if ev.RRule == "" {
				ev.RRule = strings.TrimSpace(cl.value)
			}
		case "ORGANIZER":
			ev.Organizer = addressOf(*cl)
		case "ATTENDEE":
			a := Attendee{Address: addressOf(*cl)}
			if a.Email == "" {
				continue
			}
			a.PartStat = strings.ToUpper(cl.params["PARTSTAT"])
			if a.PartStat == "" {
				a.PartStat = NeedsAction
			}
			a.Optional = strings.EqualFold(cl.params["ROLE"], "OPT-PARTICIPANT") || strings.EqualFold(cl.params["ROLE"], "NON-PARTICIPANT")
			a.RSVP = strings.EqualFold(cl.params["RSVP"], "TRUE")
			ev.Attendees = append(ev.Attendees, a)
		case "CONFERENCE", "X-GOOGLE-CONFERENCE", "X-MICROSOFT-SKYPETEAMSMEETINGURL":
			if u := strings.TrimSpace(cl.value); ev.Conference == "" && isLink(u) {
				ev.Conference = u
			}
		case "ATTACH":
			// A file's link; a file written into the calendar (base64) isn't kept.
			if u := strings.TrimSpace(cl.value); isLink(u) && !strings.EqualFold(cl.params["ENCODING"], "BASE64") {
				name := cl.params["FILENAME"]
				if name == "" {
					name = cl.params["X-FILENAME"]
				}
				ev.Attachments = append(ev.Attachments, Attachment{Name: name, URL: u})
			}
		case "X-ONECAMP-ALARM":
			if a, ok := alarmOf(cl.alarm); ok {
				ev.Alarms = append(ev.Alarms, a)
			}
		}
	}
	what := ev.Summary
	if what == "" {
		what = "An event with no title"
	}
	if start == nil {
		return ev, &Skip{What: what, Why: "It has no start."}
	}
	s, err := timeOf(*start, cal, zones)
	if err != nil {
		return ev, &Skip{What: what, Why: "Its start couldn't be read: " + err.Error()}
	}
	what += " on " + s.at.Format("2 Jan 2006")
	if ev.UID == "" {
		return ev, &Skip{What: what, Why: "It has no UID, so its changes couldn't be matched to it."}
	}
	if s.date {
		ev.StartDate = s.at.Format("2006-01-02")
	} else {
		ev.Start, ev.Zone = s.at, s.zone
	}
	switch {
	case end != nil:
		e, err := timeOf(*end, cal, zones)
		if err != nil {
			return ev, &Skip{What: what, Why: "Its end couldn't be read: " + err.Error()}
		}
		if e.date != s.date {
			return ev, &Skip{What: what, Why: "Its start and end aren't both dates or both times."}
		}
		if s.date {
			ev.EndDate = e.at.Format("2006-01-02")
		} else {
			ev.End = e.at
		}
	case duration != "":
		d, err := parseDuration(duration)
		if err != nil {
			return ev, &Skip{What: what, Why: "Its length couldn't be read."}
		}
		if s.date {
			ev.EndDate = s.at.Add(d).Format("2006-01-02")
		} else {
			ev.End = s.at.Add(d)
		}
	default:
		// RFC 5545 3.6.1: a date lasts the day, a time takes no time.
		if s.date {
			ev.EndDate = s.at.AddDate(0, 0, 1).Format("2006-01-02")
		} else {
			ev.End = s.at
		}
	}
	if s.date && ev.EndDate <= ev.StartDate {
		ev.EndDate = s.at.AddDate(0, 0, 1).Format("2006-01-02")
	}
	if !s.date && ev.End.Before(ev.Start) {
		return ev, &Skip{What: what, Why: "It ends before it starts."}
	}
	for i := range lines {
		cl := lines[i]
		switch cl.name {
		case "RECURRENCE-ID":
			t, err := timeOf(cl, cal, zones)
			if err != nil {
				return ev, &Skip{What: what, Why: "The instance of its series it changes couldn't be read."}
			}
			at := t.at
			ev.RecurrenceID = &at
		case "EXDATE", "RDATE":
			if strings.EqualFold(cl.params["VALUE"], "PERIOD") {
				cal.Skipped = append(cal.Skipped, Skip{What: what, Why: "Extra dates given as periods were left out."})
				continue
			}
			for _, v := range strings.Split(cl.value, ",") {
				one := cl
				one.value = strings.TrimSpace(v)
				t, err := timeOf(one, cal, zones)
				if err != nil {
					continue
				}
				if cl.name == "EXDATE" {
					ev.ExDates = append(ev.ExDates, t.at)
				} else {
					ev.RDates = append(ev.RDates, t.at)
				}
			}
		}
	}
	return ev, nil
}

// when is a DTSTART-like value read: an instant, or a date (at 00:00 UTC),
// and the IANA zone it was given in ("" for UTC or a date).
type whenRead struct {
	at   time.Time
	date bool
	zone string
}

func timeOf(cl contentLine, cal *Calendar, zones map[string]*time.Location) (whenRead, error) {
	v := strings.TrimSpace(cl.value)
	if strings.EqualFold(cl.params["VALUE"], "DATE") || (len(v) == 8 && !strings.Contains(v, "T")) {
		t, err := time.Parse("20060102", v)
		if err != nil {
			return whenRead{}, fmt.Errorf("%q isn't a date", v)
		}
		return whenRead{at: t, date: true}, nil
	}
	if strings.HasSuffix(v, "Z") {
		t, err := time.Parse("20060102T150405Z", v)
		if err != nil {
			return whenRead{}, fmt.Errorf("%q isn't a time", v)
		}
		return whenRead{at: t}, nil
	}
	var loc *time.Location
	zone := ""
	if id := cl.params["TZID"]; id != "" {
		if l := zoneNamed(id); l != nil {
			loc = l
		} else if l := zones[id]; l != nil {
			loc = l // a zone of the file's own, read at its standard offset
		} else {
			return whenRead{}, fmt.Errorf("the time zone %q isn't known", id)
		}
	} else if cal.Zone != "" {
		loc, _ = time.LoadLocation(cal.Zone)
	}
	if loc == nil {
		loc = time.UTC
	}
	if loc != time.UTC && !strings.HasPrefix(loc.String(), "UTC") {
		zone = loc.String() // an IANA name; a fixed offset keeps no zone
	}
	t, err := time.ParseInLocation("20060102T150405", v, loc)
	if err != nil {
		return whenRead{}, fmt.Errorf("%q isn't a time", v)
	}
	return whenRead{at: t.UTC(), zone: zone}, nil
}

// zoneNamed is the zone a TZID names, as Go knows it: an IANA name, quoted or
// behind a path ("/mozilla.org/20050126_1/Europe/London"), or a Windows name
// as Outlook writes it. Nil when it names none.
func zoneNamed(id string) *time.Location {
	id = strings.Trim(strings.TrimSpace(id), `"`)
	if id == "" {
		return nil
	}
	if strings.EqualFold(id, "UTC") || strings.EqualFold(id, "GMT") || strings.EqualFold(id, "Z") {
		return time.UTC
	}
	if iana, ok := windowsZones[id]; ok {
		id = iana
	}
	if loc, err := time.LoadLocation(id); err == nil {
		return loc
	}
	// A path in front: try the last two parts, then three ("America/Argentina/Buenos_Aires").
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for n := 2; n <= 3 && n <= len(parts); n++ {
		if loc, err := time.LoadLocation(strings.Join(parts[len(parts)-n:], "/")); err == nil {
			return loc
		}
	}
	return nil
}

// readZone reads a VTIMEZONE whose TZID Go doesn't know as a zone with a fixed
// offset: its standard time's. A calendar with such a zone is rare (a custom
// one); a fixed offset is right outside its daylight months, and the import
// says so.
func readZone(lines []contentLine) (string, *time.Location) {
	id, off := "", ""
	for _, cl := range lines {
		switch {
		case cl.name == "TZID" && cl.section == "VTIMEZONE":
			id = strings.TrimSpace(cl.value)
		case cl.name == "TZOFFSETTO" && cl.section == "STANDARD" && off == "":
			off = strings.TrimSpace(cl.value)
		}
	}
	if id == "" {
		return "", nil
	}
	if loc := zoneNamed(id); loc != nil {
		return id, loc
	}
	secs, ok := offsetSeconds(off)
	if !ok {
		return id, nil
	}
	return id, time.FixedZone("UTC"+off, secs)
}

func offsetSeconds(s string) (int, bool) {
	if len(s) != 5 && len(s) != 7 {
		return 0, false
	}
	sign := 1
	switch s[0] {
	case '-':
		sign = -1
	case '+':
	default:
		return 0, false
	}
	h, err1 := strconv.Atoi(s[1:3])
	m, err2 := strconv.Atoi(s[3:5])
	sec := 0
	var err3 error
	if len(s) == 7 {
		sec, err3 = strconv.Atoi(s[5:7])
	}
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return sign * (h*3600 + m*60 + sec), true
}

// parseDuration reads an RFC 5545 duration ("PT1H30M", "P1D", "-PT15M", "P2W").
func parseDuration(s string) (time.Duration, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	if !strings.HasPrefix(s, "P") || len(s) < 3 {
		return 0, fmt.Errorf("not a duration")
	}
	s = s[1:]
	var d time.Duration
	inTime := false
	num := ""
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			num += string(r)
		case r == 'T':
			inTime = true
		default:
			n, err := strconv.Atoi(num)
			if err != nil {
				return 0, fmt.Errorf("not a duration")
			}
			num = ""
			switch {
			case r == 'W' && !inTime:
				d += time.Duration(n) * 7 * 24 * time.Hour
			case r == 'D' && !inTime:
				d += time.Duration(n) * 24 * time.Hour
			case r == 'H' && inTime:
				d += time.Duration(n) * time.Hour
			case r == 'M' && inTime:
				d += time.Duration(n) * time.Minute
			case r == 'S' && inTime:
				d += time.Duration(n) * time.Second
			default:
				return 0, fmt.Errorf("not a duration")
			}
		}
	}
	if num != "" {
		return 0, fmt.Errorf("not a duration")
	}
	if neg {
		d = -d
	}
	return d, nil
}

// addressOf reads an ORGANIZER or ATTENDEE: its name and address.
func addressOf(cl contentLine) Address {
	v := strings.TrimSpace(cl.value)
	if len(v) >= 7 && strings.EqualFold(v[:7], "mailto:") {
		v = v[7:]
	} else if strings.Contains(v, ":") {
		v = "" // a URN or another scheme: no address mail reaches
	}
	return Address{Name: cl.params["CN"], Email: strings.TrimSpace(v)}
}

// alarmOf reads a VALARM triggered a time before (or after) the event's
// start. An alarm at a fixed time, or relative to the end, is left out.
func alarmOf(lines []contentLine) (Alarm, bool) {
	var a Alarm
	found := false
	for _, cl := range lines {
		switch cl.name {
		case "ACTION":
			a.Email = strings.EqualFold(strings.TrimSpace(cl.value), "EMAIL")
		case "TRIGGER":
			if strings.EqualFold(cl.params["VALUE"], "DATE-TIME") || strings.EqualFold(cl.params["RELATED"], "END") {
				return a, false
			}
			d, err := parseDuration(cl.value)
			if err != nil {
				return a, false
			}
			a.Before, found = -d, true
		}
	}
	if a.Before < 0 {
		a.Before = 0
	}
	return a, found
}

// isLink is whether s is a web address a person can open.
func isLink(s string) bool {
	l := strings.ToLower(s)
	return (strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")) && !strings.ContainsAny(s, " \t\r\n")
}

// ReadRecurrence reads a series' recurrence as a provider sends it apart
// from its event (Google's "recurrence": RRULE, EXDATE and RDATE lines), its
// times in zone when a line names none. Lines it can't read are left out.
func ReadRecurrence(lines []string, zone string) (rule string, exdates, rdates []time.Time) {
	cal := &Calendar{}
	if loc := zoneNamed(zone); loc != nil && loc != time.UTC {
		cal.Zone = loc.String()
	}
	for _, raw := range lines {
		cl, ok := parseLine(strings.TrimSpace(raw))
		if !ok {
			continue
		}
		switch cl.name {
		case "RRULE":
			if rule == "" {
				rule = strings.TrimSpace(cl.value)
			}
		case "EXDATE", "RDATE":
			for _, v := range strings.Split(cl.value, ",") {
				one := cl
				one.value = strings.TrimSpace(v)
				t, err := timeOf(one, cal, nil)
				if err != nil {
					continue
				}
				if cl.name == "EXDATE" {
					exdates = append(exdates, t.at)
				} else {
					rdates = append(rdates, t.at)
				}
			}
		}
	}
	return rule, exdates, rdates
}
