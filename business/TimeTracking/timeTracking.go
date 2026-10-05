// Package business (TimeTracking) is time on tasks: what a person may enter,
// and how a project's entries add up for a report or an invoice. Everything
// here is pure; storage is models/postgres/TimeEntry.
package business

import (
	"encoding/csv"
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

// Line is one person's or one task's share of a report.
type Line struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Seconds         int64  `json:"seconds"`
	BillableSeconds int64  `json:"billable_seconds"`
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
}

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

func addTo(lines map[string]*Line, id, name string, secs int64, billable bool) {
	l := lines[id]
	if l == nil {
		l = &Line{ID: id, Name: name}
		lines[id] = l
	}
	l.Seconds += secs
	if billable {
		l.BillableSeconds += secs
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
// first. Running timers count up to now.
func Summarise(entries []timeModel.Entry, names Names, from, to, now time.Time) Report {
	r := Report{From: from, To: to, Entries: len(entries)}
	if len(entries) > timeModel.MaxReportRows {
		entries, r.Truncated, r.Entries = entries[:timeModel.MaxReportRows], true, timeModel.MaxReportRows
	}
	people, tasks := map[string]*Line{}, map[string]*Line{}
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
		addTo(people, e.UserID.String(), names.person(e.UserID), secs, e.Billable)
		addTo(tasks, e.TaskUUID.String(), names.task(e.TaskUUID), secs, e.Billable)
	}
	r.ByPerson, r.ByTask = sorted(people), sorted(tasks)
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

// WriteCSV writes one row per entry, oldest first, in the time zone given.
func WriteCSV(w io.Writer, entries []timeModel.Entry, names Names, loc *time.Location, now time.Time) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"Date", "Start", "End", "Person", "Task", "Hours", "Billable", "Note"}); err != nil {
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
		if err := cw.Write([]string{
			start.Format("2006-01-02"), start.Format("15:04"), end,
			csvCell(names.person(e.UserID)), csvCell(names.task(e.TaskUUID)),
			Hours(e.Seconds(now)), billable, csvCell(e.Note),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
