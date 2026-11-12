package microsoft

// The codec: a Microsoft Graph event (v1.0) read as the codec's event
// (business/ICS), so it comes into OneCamp through the calendar's one import
// path as a file does, and the codec's event written as Graph takes it. Pure.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	ICS "github.com/akashc777/OneCamp/business/ICS"
)

// graphEvent is an event as Graph sends it.
type graphEvent struct {
	ID                         string          `json:"id"`
	ICalUID                    string          `json:"iCalUId"`
	ChangeKey                  string          `json:"changeKey"`
	Type                       string          `json:"type"`
	SeriesMasterID             string          `json:"seriesMasterId"`
	Subject                    string          `json:"subject"`
	Body                       *itemBody       `json:"body"`
	Start                      *dateTimeZone   `json:"start"`
	End                        *dateTimeZone   `json:"end"`
	OriginalStart              string          `json:"originalStart"`
	OriginalStartTimeZone      string          `json:"originalStartTimeZone"`
	IsAllDay                   bool            `json:"isAllDay"`
	IsCancelled                bool            `json:"isCancelled"`
	ShowAs                     string          `json:"showAs"`
	Sensitivity                string          `json:"sensitivity"`
	IsReminderOn               bool            `json:"isReminderOn"`
	ReminderMinutesBeforeStart int             `json:"reminderMinutesBeforeStart"`
	Location                   *graphLocation  `json:"location"`
	Organizer                  *graphRecipient `json:"organizer"`
	Attendees                  []graphAttendee `json:"attendees"`
	OnlineMeeting              *struct {
		JoinURL string `json:"joinUrl"`
	} `json:"onlineMeeting"`
	LastModified         string               `json:"lastModifiedDateTime"`
	Recurrence           *patternedRecurrence `json:"recurrence"`
	CancelledOccurrences []string             `json:"cancelledOccurrences"`
	ExceptionOccurrences []graphEvent         `json:"exceptionOccurrences"`
	// Removed marks an item of a delta that is gone from the window.
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

type itemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type dateTimeZone struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type graphLocation struct {
	DisplayName string `json:"displayName"`
}

type emailAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type graphRecipient struct {
	EmailAddress emailAddress `json:"emailAddress"`
}

type graphAttendee struct {
	Type   string `json:"type"`
	Status *struct {
		Response string `json:"response"`
	} `json:"status"`
	EmailAddress emailAddress `json:"emailAddress"`
}

type patternedRecurrence struct {
	Pattern recurrencePattern `json:"pattern"`
	Range   recurrenceRange   `json:"range"`
}

type recurrencePattern struct {
	Type           string   `json:"type"`
	Interval       int      `json:"interval"`
	Month          int      `json:"month,omitempty"`
	DayOfMonth     int      `json:"dayOfMonth,omitempty"`
	DaysOfWeek     []string `json:"daysOfWeek,omitempty"`
	FirstDayOfWeek string   `json:"firstDayOfWeek,omitempty"`
	Index          string   `json:"index,omitempty"`
}

type recurrenceRange struct {
	Type                string `json:"type"`
	StartDate           string `json:"startDate"`
	EndDate             string `json:"endDate,omitempty"`
	NumberOfOccurrences int    `json:"numberOfOccurrences,omitempty"`
	RecurrenceTimeZone  string `json:"recurrenceTimeZone,omitempty"`
}

// graphWrite is an event as OneCamp sends it. Guests are left out: Graph
// emails every attendee of an event it is given, and OneCamp tells its
// guests itself (as Google's import does).
type graphWrite struct {
	Subject                    string               `json:"subject"`
	Body                       itemBody             `json:"body"`
	Start                      dateTimeZone         `json:"start"`
	End                        dateTimeZone         `json:"end"`
	IsAllDay                   bool                 `json:"isAllDay"`
	Location                   graphLocation        `json:"location"`
	ShowAs                     string               `json:"showAs"`
	Sensitivity                string               `json:"sensitivity"`
	IsReminderOn               bool                 `json:"isReminderOn"`
	ReminderMinutesBeforeStart int                  `json:"reminderMinutesBeforeStart,omitempty"`
	Recurrence                 *patternedRecurrence `json:"recurrence,omitempty"`
}

// errUnrepeatable is a rule Outlook has no pattern for.
var errUnrepeatable error = words("Outlook can't repeat an event that way")

const graphTime = "2006-01-02T15:04:05.9999999"

// graphAnswers are Graph's responses as iCalendar says them.
var graphAnswers = map[string]string{"accepted": ICS.Accepted, "declined": ICS.Declined, "tentativelyAccepted": ICS.Tentative}

var (
	days    = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	dayCode = map[string]string{"sunday": "SU", "monday": "MO", "tuesday": "TU", "wednesday": "WE", "thursday": "TH", "friday": "FR", "saturday": "SA"}
	indexes = map[string]int{"first": 1, "second": 2, "third": 3, "fourth": 4, "last": -1}
)

// eventOf is a Graph event as the codec's event; zone is its calendar's,
// for an event that names none it knows. Pure.
func eventOf(g graphEvent, zone string) ICS.Event {
	out := ICS.Event{UID: g.ICalUID, Summary: g.Subject, Description: bodyText(g.Body)}
	if out.UID == "" {
		out.UID = g.ID + "@outlook.com"
	}
	if g.Location != nil {
		out.Location = g.Location.DisplayName
	}
	if t, err := time.Parse(time.RFC3339, g.LastModified); err == nil {
		out.Stamp = t.UTC()
	}
	if z := ianaZone(g.OriginalStartTimeZone); z != "" {
		zone = z
	}
	if g.IsAllDay {
		out.StartDate, out.EndDate = datePart(g.Start), datePart(g.End)
	} else {
		out.Start, out.End, out.Zone = instant(g.Start), instant(g.End), zone
	}
	out.Transparent = g.ShowAs == "free" || g.ShowAs == "workingElsewhere"
	// Private and confidential are who may see it; personal is a label.
	if g.Sensitivity == "private" || g.Sensitivity == "confidential" {
		out.Class = strings.ToUpper(g.Sensitivity)
	}
	if g.IsCancelled {
		out.Status = "CANCELLED"
	}
	if g.IsReminderOn && g.ReminderMinutesBeforeStart >= 0 {
		out.Alarms = []ICS.Alarm{{Before: time.Duration(g.ReminderMinutesBeforeStart) * time.Minute}}
	}
	if o := g.Organizer; o != nil && o.EmailAddress.Address != "" {
		out.Organizer = ICS.Address{Name: o.EmailAddress.Name, Email: o.EmailAddress.Address}
	}
	for _, a := range g.Attendees {
		if a.Type == "resource" || a.EmailAddress.Address == "" {
			continue // a room is where it is, not a guest
		}
		stat := ICS.NeedsAction
		if a.Status != nil && graphAnswers[a.Status.Response] != "" {
			stat = graphAnswers[a.Status.Response]
		}
		out.Attendees = append(out.Attendees, ICS.Attendee{Address: ICS.Address{Name: a.EmailAddress.Name, Email: a.EmailAddress.Address},
			Optional: a.Type == "optional", PartStat: stat})
	}
	if g.OnlineMeeting != nil {
		out.Conference = g.OnlineMeeting.JoinURL
	}
	if g.Recurrence != nil {
		if rule, err := ruleOf(*g.Recurrence, out, zone); err == nil {
			out.RRule = rule
		}
		for _, oid := range g.CancelledOccurrences {
			if d, ok := occurrenceDate(oid); ok {
				out.ExDates = append(out.ExDates, onDay(d, out, zone))
			}
		}
	}
	if g.OriginalStart != "" {
		if t, err := time.Parse(time.RFC3339, g.OriginalStart); err == nil {
			rid := t.UTC()
			if g.IsAllDay {
				l := t.In(location(zone))
				rid = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
			}
			out.RecurrenceID = &rid
		}
	}
	return out
}

// bodyText is an event's body as text: HTML as its words. Pure.
func bodyText(b *itemBody) string {
	if b == nil {
		return ""
	}
	if !strings.EqualFold(b.ContentType, "html") {
		return b.Content
	}
	doc, err := contentcodec.FromHTML(b.Content, contentcodec.HTMLOptions{Foreign: true})
	if err != nil {
		return b.Content
	}
	return strings.TrimSpace(contentcodec.PlainText(doc))
}

func datePart(d *dateTimeZone) string {
	if d == nil || len(d.DateTime) < 10 {
		return ""
	}
	return d.DateTime[:10]
}

// instant is a Graph time in the zone it names, in UTC.
func instant(d *dateTimeZone) time.Time {
	if d == nil {
		return time.Time{}
	}
	t, err := time.ParseInLocation(graphTime, d.DateTime, location(ianaZone(d.TimeZone)))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// occurrenceDate is the day an occurrence id names ("OID.<series>.<day>").
func occurrenceDate(oid string) (time.Time, bool) {
	i := strings.LastIndexByte(oid, '.')
	if i < 0 {
		return time.Time{}, false
	}
	d, err := time.Parse("2006-01-02", oid[i+1:])
	return d, err == nil
}

// onDay is a series' instance on day d: at the series' time of day in its
// zone, or for an all-day series the day itself at 00:00 UTC.
func onDay(d time.Time, ev ICS.Event, zone string) time.Time {
	if ev.StartDate != "" {
		return d
	}
	l := location(zone)
	s := ev.Start.In(l)
	return time.Date(d.Year(), d.Month(), d.Day(), s.Hour(), s.Minute(), s.Second(), 0, l).UTC()
}

// ruleOf is Graph's pattern as an RRULE. Pure.
func ruleOf(r patternedRecurrence, ev ICS.Event, zone string) (string, error) {
	p := r.Pattern
	interval := max(p.Interval, 1)
	byDay := func(prefix string) string {
		var codes []string
		for _, d := range p.DaysOfWeek {
			codes = append(codes, prefix+dayCode[strings.ToLower(d)])
		}
		return strings.Join(codes, ",")
	}
	parts := []string{}
	switch p.Type {
	case "daily":
		parts = append(parts, "FREQ=DAILY", "INTERVAL="+strconv.Itoa(interval))
	case "weekly":
		parts = append(parts, "FREQ=WEEKLY", "INTERVAL="+strconv.Itoa(interval), "BYDAY="+byDay(""))
		if c := dayCode[strings.ToLower(p.FirstDayOfWeek)]; c != "" {
			parts = append(parts, "WKST="+c)
		}
	case "absoluteMonthly":
		parts = append(parts, "FREQ=MONTHLY", "INTERVAL="+strconv.Itoa(interval), "BYMONTHDAY="+strconv.Itoa(p.DayOfMonth))
	case "relativeMonthly":
		parts = append(parts, "FREQ=MONTHLY", "INTERVAL="+strconv.Itoa(interval), "BYDAY="+byDay(""), "BYSETPOS="+strconv.Itoa(indexOr(p.Index)))
	case "absoluteYearly":
		parts = append(parts, "FREQ=YEARLY", "INTERVAL="+strconv.Itoa(interval), "BYMONTH="+strconv.Itoa(p.Month), "BYMONTHDAY="+strconv.Itoa(p.DayOfMonth))
	case "relativeYearly":
		parts = append(parts, "FREQ=YEARLY", "INTERVAL="+strconv.Itoa(interval), "BYMONTH="+strconv.Itoa(p.Month), "BYDAY="+byDay(""), "BYSETPOS="+strconv.Itoa(indexOr(p.Index)))
	default:
		return "", fmt.Errorf("%w: %q", errUnrepeatable, p.Type)
	}
	switch r.Range.Type {
	case "numbered":
		parts = append(parts, "COUNT="+strconv.Itoa(r.Range.NumberOfOccurrences))
	case "endDate":
		end, err := time.Parse("2006-01-02", r.Range.EndDate)
		if err != nil {
			return "", err
		}
		if ev.StartDate != "" {
			parts = append(parts, "UNTIL="+end.Format("20060102"))
		} else {
			l := location(zone)
			parts = append(parts, "UNTIL="+time.Date(end.Year(), end.Month(), end.Day(), 23, 59, 59, 0, l).UTC().Format("20060102T150405Z"))
		}
	}
	return strings.Join(parts, ";"), nil
}

func indexOr(index string) int {
	if i, ok := indexes[index]; ok {
		return i
	}
	return 1
}

// graphEventOf is the codec's event as Graph takes it: a rule Outlook has no
// pattern for is errUnrepeatable. Pure.
func graphEventOf(ev ICS.Event) (graphWrite, error) {
	out := graphWrite{Subject: ev.Summary, Body: itemBody{ContentType: "text", Content: ev.Description}, Location: graphLocation{DisplayName: ev.Location}, ShowAs: "busy", Sensitivity: "normal"}
	if ev.Class == "PRIVATE" || ev.Class == "CONFIDENTIAL" {
		out.Sensitivity = strings.ToLower(ev.Class)
	}
	if ev.Transparent {
		out.ShowAs = "free"
	}
	if len(ev.Alarms) > 0 {
		out.IsReminderOn, out.ReminderMinutesBeforeStart = true, int(ev.Alarms[0].Before/time.Minute)
	}
	win := windowsZone(ev.Zone)
	l := location(ev.Zone)
	if win == "" {
		win, l = "UTC", time.UTC // a zone Outlook has no name for goes in UTC
	}
	startDay := ""
	if ev.StartDate != "" {
		out.IsAllDay, win = true, "UTC"
		out.Start = dateTimeZone{DateTime: ev.StartDate + "T00:00:00", TimeZone: win}
		out.End = dateTimeZone{DateTime: ev.EndDate + "T00:00:00", TimeZone: win}
		startDay = ev.StartDate
	} else {
		out.Start = dateTimeZone{DateTime: ev.Start.In(l).Format("2006-01-02T15:04:05"), TimeZone: win}
		out.End = dateTimeZone{DateTime: ev.End.In(l).Format("2006-01-02T15:04:05"), TimeZone: win}
		startDay = ev.Start.In(l).Format("2006-01-02")
	}
	if ev.RRule != "" && ev.RecurrenceID == nil {
		r, err := recurrenceOf(ev.RRule, ev, l, startDay)
		if err != nil {
			return graphWrite{}, err
		}
		r.Range.RecurrenceTimeZone = win
		out.Recurrence = &r
	}
	return out, nil
}

// recurrenceOf is an RRULE as Graph's pattern. Pure.
func recurrenceOf(rule string, ev ICS.Event, l *time.Location, startDay string) (patternedRecurrence, error) {
	kv := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(strings.ToUpper(rule), "RRULE:"), ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			kv[k] = v
		}
	}
	bad := func(why string) (patternedRecurrence, error) {
		return patternedRecurrence{}, fmt.Errorf("%w: %s", errUnrepeatable, why)
	}
	for _, k := range []string{"BYHOUR", "BYMINUTE", "BYSECOND", "BYWEEKNO", "BYYEARDAY"} {
		if kv[k] != "" {
			return bad(k)
		}
	}
	start := ev.Start.In(l)
	if ev.StartDate != "" {
		start, _ = time.Parse("2006-01-02", ev.StartDate)
	}
	p := recurrencePattern{Interval: 1, FirstDayOfWeek: "monday"}
	if n, err := strconv.Atoi(kv["INTERVAL"]); err == nil && n > 0 {
		p.Interval = n
	}
	if w := kv["WKST"]; w != "" {
		for name, code := range dayCode {
			if code == w {
				p.FirstDayOfWeek = name
			}
		}
	}
	// BYDAY: plain days, or days with one ordinal shared by all ("-1FR").
	ordinal, ordinals := 0, map[int]bool{}
	for _, d := range strings.Split(kv["BYDAY"], ",") {
		if d == "" {
			continue
		}
		code := d[len(d)-min(2, len(d)):]
		n, _ := strconv.Atoi(strings.TrimSuffix(d, code))
		ordinal, ordinals[n] = n, true
		name := ""
		for k, c := range dayCode {
			if c == code {
				name = k
			}
		}
		if name == "" {
			return bad("BYDAY " + d)
		}
		p.DaysOfWeek = append(p.DaysOfWeek, name)
	}
	if len(ordinals) > 1 {
		return bad("BYDAY with more than one position")
	}
	if pos := kv["BYSETPOS"]; pos != "" {
		n, err := strconv.Atoi(pos)
		if err != nil || ordinal != 0 {
			return bad("BYSETPOS " + pos)
		}
		ordinal = n
	}
	index := ""
	if ordinal != 0 {
		for name, i := range indexes {
			if i == ordinal {
				index = name
			}
		}
		if index == "" {
			return bad("the position " + strconv.Itoa(ordinal))
		}
	}
	monthDay := func() (int, error) {
		if v := kv["BYMONTHDAY"]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%w: BYMONTHDAY %s", errUnrepeatable, v)
			}
			return n, nil
		}
		return start.Day(), nil
	}
	switch kv["FREQ"] {
	case "DAILY":
		p.Type = "daily"
		if len(p.DaysOfWeek) > 0 {
			p.Type = "weekly"
		}
	case "WEEKLY":
		p.Type = "weekly"
		if len(p.DaysOfWeek) == 0 {
			p.DaysOfWeek = []string{days[start.Weekday()]}
		}
	case "MONTHLY", "YEARLY":
		p.Type = "absoluteMonthly"
		if index != "" {
			p.Type, p.Index = "relativeMonthly", index
		} else if len(p.DaysOfWeek) > 0 {
			return bad("BYDAY with no position")
		} else {
			d, err := monthDay()
			if err != nil {
				return patternedRecurrence{}, err
			}
			p.DayOfMonth = d
		}
		if kv["FREQ"] == "YEARLY" {
			p.Type = strings.Replace(p.Type, "Monthly", "Yearly", 1)
			p.Month = int(start.Month())
			if m, err := strconv.Atoi(kv["BYMONTH"]); err == nil {
				p.Month = m
			}
		}
	default:
		return bad("FREQ " + kv["FREQ"])
	}
	r := patternedRecurrence{Pattern: p, Range: recurrenceRange{Type: "noEnd", StartDate: startDay}}
	if c, err := strconv.Atoi(kv["COUNT"]); err == nil && c > 0 {
		r.Range.Type, r.Range.NumberOfOccurrences = "numbered", c
	} else if u := kv["UNTIL"]; u != "" {
		var until time.Time
		var err error
		if len(u) == 8 {
			until, err = time.Parse("20060102", u)
		} else {
			until, err = time.Parse("20060102T150405Z", u)
			until = until.In(l)
		}
		if err != nil {
			return bad("UNTIL " + u)
		}
		r.Range.Type, r.Range.EndDate = "endDate", until.Format("2006-01-02")
	}
	return r, nil
}
