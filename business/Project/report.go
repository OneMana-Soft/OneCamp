package business

// The report: how work is going across the projects someone is in, for the
// weeks up to this one. How much got done and how much was added each week,
// what's open and overdue by project, by person and by priority, and the hours
// logged. Asana keeps portfolio dashboards for its Advanced plan, monday its
// richer dashboards for Pro, ClickUp its reporting widgets for paid plans; the
// questions a manager brings to a weekly review are the same few, so these
// answer them without building a dashboard first.

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskStatusModels "github.com/akashc777/OneCamp/models/postgres/TaskStatus"
	timeModels "github.com/akashc777/OneCamp/models/postgres/TimeEntry"
	"github.com/google/uuid"
)

// ReportWeeks is how many weeks a report covers unless asked; MaxReportWeeks the most.
const (
	ReportWeeks    = 12
	MaxReportWeeks = 52
)

// ReportCounts is a row of the report: its open tasks by where they stand,
// how many of them are overdue, and how many of its tasks got done in the
// weeks shown.
type ReportCounts struct {
	ToDo       int `json:"to_do"`
	InProgress int `json:"in_progress"`
	InReview   int `json:"in_review"`
	Overdue    int `json:"overdue"`
	Done       int `json:"done"`
}

// Open is how many tasks of the row are open.
func (c ReportCounts) Open() int { return c.ToDo + c.InProgress + c.InReview }

// ReportProjectRow is one project's row.
type ReportProjectRow struct {
	UUID string `json:"project_uuid"`
	Name string `json:"project_name"`
	ReportCounts
}

// ReportPersonRow is one person's row; user_uuid "" is the tasks nobody has
// (unassigned, or whoever had them was deleted).
type ReportPersonRow struct {
	UUID       string  `json:"user_uuid,omitempty"`
	Name       string  `json:"user_name,omitempty"`
	FullName   string  `json:"user_full_name,omitempty"`
	ProfileKey *string `json:"user_profile_object_key,omitempty"`
	ReportCounts
}

// ReportPriorityRow is the open tasks of one priority ("" for none).
type ReportPriorityRow struct {
	Priority string `json:"priority"`
	Open     int    `json:"open"`
	Overdue  int    `json:"overdue"`
}

// ReportProjectRef is a project the report can cover, for choosing which.
type ReportProjectRef struct {
	UUID string `json:"project_uuid"`
	Name string `json:"project_name"`
}

// Report is what the reports view draws. The weekly lists line up with weeks
// (each week's Monday in the reader's zone, oldest first). hours is nil when
// the time logged couldn't be read; on_time_percent is left out when nothing
// done in the weeks shown had a due date.
type Report struct {
	Weeks       []string            `json:"weeks"`
	Done        []int               `json:"done"`
	Added       []int               `json:"added"`
	Hours       []float64           `json:"hours"`
	Open        int                 `json:"open"`
	Overdue     int                 `json:"overdue"`
	DueThisWeek int                 `json:"due_this_week"`
	DoneTotal   int                 `json:"done_total"`
	OnTime      *int                `json:"on_time_percent,omitempty"`
	Projects    []ReportProjectRow  `json:"projects"`
	People      []ReportPersonRow   `json:"people"`
	Priorities  []ReportPriorityRow `json:"priorities"`
	All         []ReportProjectRef  `json:"all_projects"`
	Truncated   bool                `json:"truncated"`
	// Flow is where the tasks stood at the end of each week (flow.go).
	Flow []FlowWeek `json:"flow"`
}

// weekStart is the Monday, at midnight in loc, of the week t falls in.
func weekStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day()-(int(t.Weekday())+6)%7, 0, 0, 0, 0, loc)
}

// priorityOrder sorts priorities from high to none.
var priorityOrder = map[string]int{dgraphStruct.TASK_PRIORITY_HIGH: 0, dgraphStruct.TASK_PRIORITY_MEDIUM: 1, dgraphStruct.TASK_PRIORITY_LOW: 2, "": 3}

// BuildReport works the report out of what was read: projects and the hours
// logged on them (nil when they couldn't be read), as of now in loc, for the
// weeks up to this one. only, when not empty, keeps to those projects. Pure.
func BuildReport(projects []domain.ReportProject, hours []timeModels.WeekSeconds, now time.Time, loc *time.Location, weeks int, only map[string]bool) *Report {
	first := weekStart(now, loc).AddDate(0, 0, -7*(weeks-1))
	today := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	r := &Report{Weeks: make([]string, weeks), Done: make([]int, weeks), Added: make([]int, weeks),
		Projects: []ReportProjectRow{}, People: []ReportPersonRow{}, Priorities: []ReportPriorityRow{}, All: []ReportProjectRef{}}
	for i := range r.Weeks {
		r.Weeks[i] = first.AddDate(0, 0, 7*i).Format(time.DateOnly)
	}
	// The week a moment falls in, or -1 before the first.
	weekOf := func(t *time.Time) int {
		if t == nil || t.Before(first) {
			return -1
		}
		i := int(weekStart(*t, loc).Sub(first).Hours()+12) / (24 * 7)
		if i >= weeks {
			return -1
		}
		return i
	}
	dated := func(t *time.Time) bool { return t != nil && t.Year() > 1970 }

	people := map[string]*ReportPersonRow{}
	priorities := map[string]*ReportPriorityRow{}
	var doneWithDue, doneOnTime int
	inReport := map[uuid.UUID]bool{}
	for _, p := range projects {
		r.All = append(r.All, ReportProjectRef{UUID: p.UUID, Name: p.Name})
		if len(only) > 0 && !only[p.UUID] {
			continue
		}
		if id, err := uuid.Parse(p.UUID); err == nil {
			inReport[id] = true
		}
		r.Truncated = r.Truncated || p.Truncated
		row := ReportProjectRow{UUID: p.UUID, Name: p.Name}
		// Whose a task is; nil for a bot's (an agent runs it, nobody's load),
		// and "" for nobody's.
		personOf := func(t *dgraphStruct.DgraphTask) *ReportPersonRow {
			u := t.Assignee
			if u != nil && u.IsBot {
				return nil
			}
			key := ""
			if u != nil && u.Uuid != "" && !dated(u.DeletedAt) {
				key = u.Uuid
			}
			pr, ok := people[key]
			if !ok {
				pr = &ReportPersonRow{UUID: key}
				if key != "" {
					pr.Name, pr.FullName, pr.ProfileKey = u.UserName, u.UserFullName, u.ProfileKey
				}
				people[key] = pr
			}
			return pr
		}
		for _, t := range p.Open {
			overdue := dated(t.DueDate) && t.DueDate.Before(today)
			count := func(c *ReportCounts) {
				switch t.Status {
				case dgraphStruct.TASK_STATUS_INPROGRESS:
					c.InProgress++
				case dgraphStruct.TASK_STATUS_INREVIEW:
					c.InReview++
				default:
					c.ToDo++
				}
				if overdue {
					c.Overdue++
				}
			}
			count(&row.ReportCounts)
			if pr := personOf(t); pr != nil {
				count(&pr.ReportCounts)
			}
			prio := strings.ToLower(t.Priority)
			if _, known := priorityOrder[prio]; !known {
				prio = ""
			}
			pp, ok := priorities[prio]
			if !ok {
				pp = &ReportPriorityRow{Priority: prio}
				priorities[prio] = pp
			}
			pp.Open++
			r.Open++
			if overdue {
				pp.Overdue++
				r.Overdue++
			} else if dated(t.DueDate) && t.DueDate.Before(today.AddDate(0, 0, 7)) {
				r.DueThisWeek++
			}
			if i := weekOf(t.CreatedAt); i >= 0 {
				r.Added[i]++
			}
		}
		for _, t := range p.Closed {
			if i := weekOf(t.CreatedAt); i >= 0 {
				r.Added[i]++
			}
			if t.Status != dgraphStruct.TASK_STATUS_DONE {
				continue
			}
			i := weekOf(t.StatusSince)
			if i < 0 {
				continue
			}
			r.Done[i]++
			r.DoneTotal++
			row.Done++
			if pr := personOf(t); pr != nil {
				pr.Done++
			}
			// On time: by the end of its due day in the reader's zone, the
			// same day the overdue count goes by.
			if dated(t.DueDate) {
				doneWithDue++
				d := t.DueDate.In(loc)
				if t.StatusSince.Before(time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc)) {
					doneOnTime++
				}
			}
		}
		if row.Open() > 0 || row.Done > 0 {
			r.Projects = append(r.Projects, row)
		}
	}
	if doneWithDue > 0 {
		pct := int(math.Round(100 * float64(doneOnTime) / float64(doneWithDue)))
		r.OnTime = &pct
	}
	for _, pr := range people {
		if pr.Open() > 0 || pr.Done > 0 {
			r.People = append(r.People, *pr)
		}
	}
	for _, pp := range priorities {
		r.Priorities = append(r.Priorities, *pp)
	}
	if hours != nil {
		r.Hours = make([]float64, weeks)
		for _, h := range hours {
			if !inReport[h.ProjectUUID] {
				continue
			}
			// The week's Monday as a date, read in the zone it was cut in.
			monday := time.Date(h.Week.Year(), h.Week.Month(), h.Week.Day(), 0, 0, 0, 0, loc)
			if i := weekOf(&monday); i >= 0 {
				r.Hours[i] += float64(h.Seconds) / 3600
			}
		}
		for i := range r.Hours {
			r.Hours[i] = math.Round(r.Hours[i]*10) / 10
		}
	}
	sort.SliceStable(r.All, func(i, j int) bool { return strings.ToLower(r.All[i].Name) < strings.ToLower(r.All[j].Name) })
	sort.SliceStable(r.Projects, func(i, j int) bool {
		return strings.ToLower(r.Projects[i].Name) < strings.ToLower(r.Projects[j].Name)
	})
	// The most open work first, nobody's last.
	sort.SliceStable(r.People, func(i, j int) bool {
		a, b := r.People[i], r.People[j]
		if (a.UUID == "") != (b.UUID == "") {
			return b.UUID == ""
		}
		if a.Open() != b.Open() {
			return a.Open() > b.Open()
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	sort.SliceStable(r.Priorities, func(i, j int) bool {
		return priorityOrder[r.Priorities[i].Priority] < priorityOrder[r.Priorities[j].Priority]
	})
	return r
}

// narrowTo is the projects a report keeps to: those asked for that are the
// reader's. A remembered choice that has gone (left, archived, or the demo's
// nightly copy) narrows nothing rather than emptying the report; empty keeps
// them all. Pure.
func narrowTo(projects []domain.ReportProject, only []string) map[string]bool {
	mine := map[string]bool{}
	for _, p := range projects {
		mine[p.UUID] = true
	}
	keep := map[string]bool{}
	for _, id := range only {
		if id = strings.TrimSpace(id); mine[id] {
			keep[id] = true
		}
	}
	return keep
}

// GetReport reads and works out the report of the reader's live projects
// (only those in only, if any) for the weeks up to now's, in loc. The hours
// are left out, not the report, if the time logged can't be read.
func GetReport(ctx context.Context, readerUID string, now time.Time, loc *time.Location, weeks int, only []string) (*Report, error) {
	if weeks < 1 || weeks > MaxReportWeeks {
		weeks = ReportWeeks
	}
	first := weekStart(now, loc).AddDate(0, 0, -7*(weeks-1))
	projects, err := domain.GetDgraphReport(ctx, readerUID, first)
	if err != nil {
		return nil, err
	}
	keep := narrowTo(projects, only)
	var ids []uuid.UUID
	for _, p := range projects {
		if id, err := uuid.Parse(p.UUID); err == nil && (len(keep) == 0 || keep[p.UUID]) {
			ids = append(ids, id)
		}
	}
	hours, err := timeModels.SecondsByWeek(ids, first, now, loc.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetReport hours err: %+v", err)
		hours = nil
	}
	r := BuildReport(projects, hours, now, loc, weeks, keep)
	r.Flow = buildFlow(projects, ownStatuses(ctx, ids), first, now, weeks, keep)
	return r, nil
}

// ownStatuses is each project's own statuses, lower-cased name → category,
// by project uuid: how a task's history names them. Without them, a history
// naming one reads as the status before it (flow.go), so a failure to read
// them costs precision, not the report.
func ownStatuses(ctx context.Context, ids []uuid.UUID) map[string]map[string]string {
	out := map[string]map[string]string{}
	byProject, err := taskStatusModels.ListForProjects(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetReport statuses err: %+v", err)
		return out
	}
	for id, statuses := range byProject {
		names := map[string]string{}
		for _, st := range statuses {
			names[strings.ToLower(strings.TrimSpace(st.Name))] = st.Category
		}
		out[id.String()] = names
	}
	return out
}
