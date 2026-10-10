package business

import (
	"fmt"
	"strings"
	"time"

	taskStatus "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
)

// The facts an update is drafted from, read from the project's tasks: what was
// finished since the last update, what is moving, what is stuck or late, and
// what comes next. The draft is these facts in words; the person edits it and
// chooses the health. (Asana's AI summary reports "on track" for work whose
// blockers were never logged; here the person decides, with the facts in
// front of them.)

// StuckDays is how long an open task can sit in a started status before the
// draft calls it stuck. It matches the board's amber "time in status".
const StuckDays = 7

// maxListed is how many tasks one section of the draft lists by name.
const maxListed = 8

// TaskFact is one task as the draft mentions it.
type TaskFact struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Assignee string     `json:"assignee,omitempty"`
	Status   string     `json:"status"`
	Due      *time.Time `json:"due,omitempty"`
	// Days is how long it has been in its status (stuck) or late (overdue).
	Days int `json:"days,omitempty"`
}

// Facts is where a project stands over a window of time.
type Facts struct {
	Project    string     `json:"project"`
	Since      time.Time  `json:"since"`
	Until      time.Time  `json:"until"`
	Done       []TaskFact `json:"done"`
	InProgress []TaskFact `json:"in_progress"`
	Stuck      []TaskFact `json:"stuck"`
	Overdue    []TaskFact `json:"overdue"`
	DueSoon    []TaskFact `json:"due_soon"`
	// Started counts the work under way however it is listed: each task is in
	// one list only, the most telling (overdue, then stuck, then in progress).
	Started int `json:"started"`
	// Created counts tasks added in the window; Open those not finished.
	Created int `json:"created"`
	Open    int `json:"open"`
	// AllDone counts every finished task, for a project with nothing open.
	AllDone int `json:"all_done"`
	// Logged is the time tracked on the project in the window, in seconds.
	Logged int64 `json:"logged_seconds"`
}

func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// real is a graph date that was set: "no date" is kept as the zero time or the epoch.
func real(t *time.Time) bool { return t != nil && t.After(time.Unix(0, 0)) }

func nameOf(u *dgraphStruct.DgraphUser) string {
	return u.DisplayName()
}

// statusName is the status as people read it: the project's own name, else the built-in one.
func statusName(t *dgraphStruct.DgraphTask) string {
	if t.CustomStatusName != nil && strings.TrimSpace(*t.CustomStatusName) != "" {
		return strings.TrimSpace(*t.CustomStatusName)
	}
	for _, b := range taskStatus.BuiltIns {
		if b.Key == t.Status {
			return b.Label
		}
	}
	return t.Status
}

// since is when a task entered its status: task_status_since, else when it was made.
func since(t *dgraphStruct.DgraphTask) (time.Time, bool) {
	if real(t.StatusSince) {
		return *t.StatusSince, true
	}
	if real(t.CreatedAt) {
		return *t.CreatedAt, true
	}
	return time.Time{}, false
}

func factOf(t *dgraphStruct.DgraphTask) TaskFact {
	f := TaskFact{ID: t.Uuid, Name: strings.TrimSpace(t.Name), Assignee: nameOf(t.Assignee), Status: statusName(t)}
	if real(t.DueDate) {
		d := *t.DueDate
		f.Due = &d
	}
	return f
}

// Gather reads the facts from a project's board (all its tasks, by status)
// for the window [from, now). logged is the time tracked in the window. Days
// are counted in now's time zone, the author's: a due date is that person's
// midnight, which in UTC can fall on the day before. Pure.
func Gather(p *dgraphStruct.DgraphProject, from, now time.Time, logged int64) Facts {
	loc := now.Location()
	from = from.In(loc)
	f := Facts{Project: p.Name, Since: from, Until: now, Logged: logged,
		Done: []TaskFact{}, InProgress: []TaskFact{}, Stuck: []TaskFact{}, Overdue: []TaskFact{}, DueSoon: []TaskFact{}}
	today := day(now)
	weekEnd := today.AddDate(0, 0, 7)
	inWindow := func(t time.Time) bool { return !t.Before(from) && t.Before(now) }

	open := [][]*dgraphStruct.DgraphTask{p.TasksBacklog, p.TasksTodo, p.TasksInProgresss, p.TasksInReview}
	for i, list := range open {
		started := i >= 2 // in progress, in review
		for _, t := range list {
			if t == nil || t.Uuid == "" {
				continue
			}
			f.Open++
			fact := factOf(t)
			if real(t.CreatedAt) && inWindow(*t.CreatedAt) {
				f.Created++
			}
			overdue := false
			if fact.Due != nil {
				local := fact.Due.In(loc)
				fact.Due = &local
				due := day(local)
				switch {
				case due.Before(today):
					late := fact
					late.Days = int(today.Sub(due).Hours() / 24)
					f.Overdue = append(f.Overdue, late)
					overdue = true
				case due.Before(weekEnd) && !started:
					// Started work shows its due date under In progress.
					f.DueSoon = append(f.DueSoon, fact)
				}
			}
			if !started {
				continue
			}
			// Once each: a late task is told as late, a stalled one as stuck.
			f.Started++
			if overdue {
				continue
			}
			if s, ok := since(t); ok {
				if d := int(now.Sub(s).Hours() / 24); d >= StuckDays {
					stuck := fact
					stuck.Days = d
					f.Stuck = append(f.Stuck, stuck)
					continue
				}
			}
			f.InProgress = append(f.InProgress, fact)
		}
	}
	for _, t := range p.TasksDone {
		if t == nil || t.Uuid == "" {
			continue
		}
		f.AllDone++
		if real(t.CreatedAt) && inWindow(*t.CreatedAt) {
			f.Created++
		}
		if s, ok := since(t); ok && inWindow(s) {
			f.Done = append(f.Done, factOf(t))
		}
	}
	if p.TasksDoneCount > f.AllDone {
		f.AllDone = p.TasksDoneCount
	}
	return f
}

// SuggestHealth is the health the facts point to; the person picks the one
// they post. Pure.
func SuggestHealth(f Facts) string {
	switch late := len(f.Overdue); {
	case f.Open == 0 && f.AllDone > 0:
		return model.Done
	case late >= 3 || (f.Open > 0 && late*4 >= f.Open && late > 0):
		return model.OffTrack
	case late > 0 || len(f.Stuck) > 0:
		return model.AtRisk
	default:
		return model.OnTrack
	}
}

// Duration is seconds as people say it: "6h 20m", "45m".
func Duration(sec int64) string {
	h, m := sec/3600, (sec%3600)/60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func section(sb *strings.Builder, title string, tasks []TaskFact, detail func(TaskFact) string) {
	if len(tasks) == 0 {
		return
	}
	sb.WriteString("\n\n" + title + ":")
	for i, t := range tasks {
		if i == maxListed {
			sb.WriteString(fmt.Sprintf("\n- and %d more", len(tasks)-maxListed))
			break
		}
		line := t.Name
		if d := detail(t); d != "" {
			line += " · " + d
		}
		if t.Assignee != "" {
			line += " (" + t.Assignee + ")"
		}
		sb.WriteString("\n- " + line)
	}
}

// DraftText is the facts as an update's text, ready to edit. Pure.
func DraftText(f Facts) string {
	var sb strings.Builder
	parts := []string{helpers.Count(len(f.Done), "task done", "tasks done"), fmt.Sprintf("%d in progress", f.Started)}
	if n := len(f.Overdue); n > 0 {
		parts = append(parts, fmt.Sprintf("%d overdue", n))
	}
	sb.WriteString(fmt.Sprintf("Since %s: %s.", f.Since.Format("Mon 2 Jan"), strings.Join(parts, ", ")))
	date := func(t *time.Time) string { return t.Format("2 Jan") }
	section(&sb, "Done", f.Done, func(TaskFact) string { return "" })
	section(&sb, "In progress", f.InProgress, func(t TaskFact) string {
		if t.Due != nil {
			return t.Status + ", due " + date(t.Due)
		}
		return t.Status
	})
	section(&sb, fmt.Sprintf("Stuck for %d days or more", StuckDays), f.Stuck, func(t TaskFact) string {
		if t.Due != nil {
			return fmt.Sprintf("%s for %d days, due %s", t.Status, t.Days, date(t.Due))
		}
		return fmt.Sprintf("%s for %d days", t.Status, t.Days)
	})
	section(&sb, "Overdue", f.Overdue, func(t TaskFact) string { return "was due " + date(t.Due) })
	section(&sb, "Not started, due in the next 7 days", f.DueSoon, func(t TaskFact) string { return "due " + date(t.Due) })
	if f.Logged > 0 {
		sb.WriteString("\n\nTime logged: " + Duration(f.Logged) + ".")
	}
	return sb.String()
}

// Window is where the next update starts counting from: the last update, if
// it was within two weeks, else a week back.
func Window(last *time.Time, now time.Time) time.Time {
	if last != nil && last.Before(now) && now.Sub(*last) <= 14*24*time.Hour {
		return *last
	}
	return now.AddDate(0, 0, -7)
}
