// Package business (TimeTracking) is time on tasks: what a person may enter,
// and how a project's entries add up for a report or an invoice. Everything
// here is pure; storage is models/postgres/TimeEntry.
package business

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	timeModel "github.com/akashc777/OneCamp/models/postgres/TimeEntry"
	"github.com/google/uuid"
)

// InputError is an entry the person can fix; its text is written for them.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

const (
	maxEntryMinutes = 24 * 60
	maxNote         = 500
	// A report covers at most a year, which an invoice never needs more than.
	maxReportSpan = 366 * 24 * time.Hour
)

// CheckSpan validates a span of work entered by hand or edited, and tidies its
// note: it lasts a minute to a day, and doesn't end in the future.
func CheckSpan(start time.Time, minutes int, note string, now time.Time) (time.Time, time.Time, string, error) {
	if start.IsZero() {
		return time.Time{}, time.Time{}, "", &InputError{"Pick when you worked."}
	}
	if minutes < 1 || minutes > maxEntryMinutes {
		return time.Time{}, time.Time{}, "", &InputError{"Enter between 1 minute and 24 hours."}
	}
	end := start.Add(time.Duration(minutes) * time.Minute)
	if end.After(now.Add(time.Minute)) {
		return time.Time{}, time.Time{}, "", &InputError{"That time hasn't happened yet."}
	}
	note = strings.Join(strings.Fields(note), " ")
	if utf8.RuneCountInString(note) > maxNote {
		return time.Time{}, time.Time{}, "", &InputError{"Keep the note under 500 characters."}
	}
	return start.UTC(), end.UTC(), note, nil
}

// ReportRange is the span a report covers: the given dates, else this month so
// far. to is exclusive.
func ReportRange(fromStr, toStr string, now time.Time) (time.Time, time.Time, error) {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := now.Add(time.Minute)
	if fromStr != "" {
		t, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			return from, to, &InputError{"That start date isn't one we can read."}
		}
		from = t
	}
	if toStr != "" {
		t, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			return from, to, &InputError{"That end date isn't one we can read."}
		}
		to = t
	}
	if !to.After(from) {
		return from, to, &InputError{"The end has to be after the start."}
	}
	if to.Sub(from) > maxReportSpan {
		return from, to, &InputError{"A report covers a year at most."}
	}
	return from, to, nil
}

// Line is one person's or one task's share of a report. With rates, the
// billable time's amount; for a person, the rate it was charged at; for a
// task, its billable time at each rate, since people on different rates may
// have worked on it (an invoice bills each at its own rate).
type Line struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Seconds         int64      `json:"seconds"`
	BillableSeconds int64      `json:"billable_seconds"`
	AmountCents     *int64     `json:"amount_cents,omitempty"`
	RateCents       *int64     `json:"rate_cents,omitempty"`
	Rated           []RatePart `json:"rated,omitempty"`
}

// RatePart is a task's billable time at one rate.
type RatePart struct {
	RateCents       int64 `json:"rate_cents"`
	BillableSeconds int64 `json:"billable_seconds"`
}

// Report is a project's time over a range.
type Report struct {
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	Seconds         int64     `json:"seconds"`
	BillableSeconds int64     `json:"billable_seconds"`
	Entries         int       `json:"entries"`
	Running         int       `json:"running"`
	ByPerson        []Line    `json:"by_person"`
	ByTask          []Line    `json:"by_task"`
	// Truncated says the range held more entries than one report reads.
	Truncated bool `json:"truncated"`
	// With rates (for the project's admins), what the billable time comes to.
	Currency    string `json:"currency,omitempty"`
	AmountCents *int64 `json:"amount_cents,omitempty"`
}

// Rates is what a project's billable time is charged at, per hour, in the
// currency's minor unit (cents, paise): a rate for anyone without their own.
type Rates struct {
	Currency string
	Default  int64
	People   map[uuid.UUID]int64
}

// For is a person's rate: their own, else the project's.
func (r *Rates) For(user uuid.UUID) int64 {
	if v, ok := r.People[user]; ok {
		return v
	}
	return r.Default
}

// Most a rate can be: a million an hour, in minor units.
const maxRateCents = 100_000_000

// RatesInput is a project's rates as its admins write them.
type RatesInput struct {
	Currency         string `json:"currency"`
	DefaultRateCents int64  `json:"default_rate_cents"`
	People           []struct {
		UserUUID  string `json:"user_uuid"`
		RateCents int64  `json:"rate_cents"`
	} `json:"people"`
}

// CheckRates is the rates as stored, or what to fix: a three-letter currency
// code, and rates from nothing to a million an hour. Pure.
func CheckRates(in RatesInput) (*Rates, error) {
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if len(cur) != 3 || strings.Trim(cur, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return nil, &InputError{"Choose the currency by its three-letter code, like USD, EUR or INR."}
	}
	ok := func(c int64) bool { return c >= 0 && c <= maxRateCents }
	if !ok(in.DefaultRateCents) {
		return nil, &InputError{"A rate is from 0 to 1,000,000 an hour."}
	}
	if len(in.People) > 500 {
		return nil, &InputError{"Set rates for up to 500 people."}
	}
	out := &Rates{Currency: cur, Default: in.DefaultRateCents, People: map[uuid.UUID]int64{}}
	for _, p := range in.People {
		id, err := uuid.Parse(strings.TrimSpace(p.UserUUID))
		if err != nil {
			return nil, &InputError{"One of those people isn't someone OneCamp knows."}
		}
		if !ok(p.RateCents) {
			return nil, &InputError{"A rate is from 0 to 1,000,000 an hour."}
		}
		out.People[id] = p.RateCents
	}
	return out, nil
}

// amount is what secs of billable work come to at rate per hour, to the
// nearest minor unit. Each entry is rounded once, so a report's lines by
// person and by task add up to the same total. Pure.
func amount(secs, rate int64) int64 { return (secs*rate + 1800) / 3600 }

// Names turns ids into what people read; a missing name has a fallback.
type Names struct {
	People map[uuid.UUID]string
	Tasks  map[uuid.UUID]string
}

func (n Names) person(id uuid.UUID) string {
	if s := n.People[id]; s != "" {
		return s
	}
	return "Former member"
}

func (n Names) task(id uuid.UUID) string {
	if s := n.Tasks[id]; s != "" {
		return s
	}
	return "Deleted task"
}

func addTo(lines map[string]*Line, id, name string, secs int64, billable bool, cents *int64) {
	l := lines[id]
	if l == nil {
		l = &Line{ID: id, Name: name}
		lines[id] = l
	}
	l.Seconds += secs
	if billable {
		l.BillableSeconds += secs
	}
	if cents != nil {
		if l.AmountCents == nil {
			l.AmountCents = new(int64)
		}
		*l.AmountCents += *cents
	}
}

func sorted(lines map[string]*Line) []Line {
	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seconds != out[j].Seconds {
			return out[i].Seconds > out[j].Seconds
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Summarise adds a project's entries up by person and by task, most time
// first. Running timers count up to now. With rates, it also says what the
// billable time comes to, overall, per person (and at what rate) and per task.
func Summarise(entries []timeModel.Entry, names Names, from, to, now time.Time, rates *Rates) Report {
	r := Report{From: from, To: to, Entries: len(entries)}
	if rates != nil {
		r.Currency, r.AmountCents = rates.Currency, new(int64)
	}
	if len(entries) > timeModel.MaxReportRows {
		entries, r.Truncated, r.Entries = entries[:timeModel.MaxReportRows], true, timeModel.MaxReportRows
	}
	people, tasks := map[string]*Line{}, map[string]*Line{}
	parts := map[string]map[int64]int64{} // task -> rate -> billable seconds
	for i := range entries {
		e := &entries[i]
		secs := e.Seconds(now)
		if e.EndedAt == nil {
			r.Running++
		}
		r.Seconds += secs
		if e.Billable {
			r.BillableSeconds += secs
		}
		var cents *int64
		if rates != nil {
			c := int64(0)
			if e.Billable {
				rate := rates.For(e.UserID)
				c = amount(secs, rate)
				task := e.TaskUUID.String()
				if parts[task] == nil {
					parts[task] = map[int64]int64{}
				}
				parts[task][rate] += secs
			}
			cents = &c
			*r.AmountCents += c
		}
		addTo(people, e.UserID.String(), names.person(e.UserID), secs, e.Billable, cents)
		addTo(tasks, e.TaskUUID.String(), names.task(e.TaskUUID), secs, e.Billable, cents)
	}
	r.ByPerson, r.ByTask = sorted(people), sorted(tasks)
	if rates != nil {
		for i := range r.ByPerson {
			if id, err := uuid.Parse(r.ByPerson[i].ID); err == nil {
				rate := rates.For(id)
				r.ByPerson[i].RateCents = &rate
			}
		}
		for i := range r.ByTask {
			for rate, secs := range parts[r.ByTask[i].ID] {
				r.ByTask[i].Rated = append(r.ByTask[i].Rated, RatePart{RateCents: rate, BillableSeconds: secs})
			}
			sort.Slice(r.ByTask[i].Rated, func(a, b int) bool { return r.ByTask[i].Rated[a].RateCents > r.ByTask[i].Rated[b].RateCents })
		}
	}
	return r
}

// Hours is seconds as hours to two places, the way an invoice reads them.
func Hours(secs int64) string {
	return strconv.FormatFloat(float64(secs)/3600, 'f', 2, 64)
}

// csvCell keeps a cell from being read as a formula by a spreadsheet: a note
// is typed by a person, and a leading = + - or @ would run when opened.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// Money is minor units as a decimal amount: 123456 is "1234.56". Pure.
func Money(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// WriteCSV writes one row per entry, oldest first, in the time zone given.
// With rates, each row also has the rate and what its billable time comes to.
func WriteCSV(w io.Writer, entries []timeModel.Entry, names Names, loc *time.Location, now time.Time, rates *Rates) error {
	cw := csv.NewWriter(w)
	head := []string{"Date", "Start", "End", "Person", "Task", "Hours", "Billable", "Note"}
	if rates != nil {
		head = append(head, "Rate ("+rates.Currency+")", "Amount ("+rates.Currency+")")
	}
	if err := cw.Write(head); err != nil {
		return err
	}
	for i := range entries {
		e := &entries[i]
		end := "running"
		if e.EndedAt != nil {
			end = e.EndedAt.In(loc).Format("15:04")
		}
		billable := "no"
		if e.Billable {
			billable = "yes"
		}
		start := e.StartedAt.In(loc)
		row := []string{
			start.Format("2006-01-02"), start.Format("15:04"), end,
			csvCell(names.person(e.UserID)), csvCell(names.task(e.TaskUUID)),
			Hours(e.Seconds(now)), billable, csvCell(e.Note),
		}
		if rates != nil {
			rate, cents := rates.For(e.UserID), int64(0)
			if e.Billable {
				cents = amount(e.Seconds(now), rate)
			}
			row = append(row, Money(rate), Money(cents))
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
