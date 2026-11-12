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
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	domain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
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
	// Insights is how long the work took (insights.go).
	Insights Insights `json:"insights"`
}

// weekStart is the Monday, at midnight in loc, of the week t falls in.
func weekStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day()-(int(t.Weekday())+6)%7, 0, 0, 0, 0, loc)
}

// dayStart is the midnight, in loc, that begins the day t falls in there:
// a task due before it is overdue.
func dayStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// BuildReport works the report out of what was read: projects and the hours
// logged on them (nil when they couldn't be read), as of now in loc, for the
// weeks up to this one. only, when not empty, keeps to those projects. Pure.
func BuildReport(projects []domain.ReportProject, hours []timeModels.WeekSeconds, now time.Time, loc *time.Location, weeks int, only map[string]bool) *Report {
	first := weekStart(now, loc).AddDate(0, 0, -7*(weeks-1))
	today := dayStart(now, loc)
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
			if !taskPriority.Is(prio) {
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
		return taskPriority.Rank(r.Priorities[i].Priority) < taskPriority.Rank(r.Priorities[j].Priority)
	})
	return r
}

// narrowTo is the projects a report keeps to: those asked for that are the
// reader's. A remembered choice that has gone (left, archived, or the demo's
// nightly copy) narrows nothing rather than emptying the report; empty keeps
// them all. Pure.
// narrowFor is narrowTo, and, when exact, whether it keeps nothing at all:
// projects were asked for and none of them is the reader's. Pure.
func narrowFor(projects []domain.ReportProject, only []string, exact bool) (keep map[string]bool, none bool) {
	keep = narrowTo(projects, only)
	return keep, exact && len(keep) == 0 && asksAny(only)
}

// asksAny is whether only names any project at all. Pure.
func asksAny(only []string) bool {
	for _, id := range only {
		if strings.TrimSpace(id) != "" {
			return true
		}
	}
	return false
}

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

// ReportAsk is what a report is asked for: its weeks, the projects it keeps
// to (none: all the reader's), and a view's filters and search, as its list
// sends them (B9-b1).
type ReportAsk struct {
	Weeks  int
	Only   []string
	Params []taskDomain.TaskListParam
	Search string
	// Exact keeps to exactly those of Only the reader is in, and to none
	// when they are in none of them: a dashboard's widget (business/
	// Dashboard), whose projects someone else chose. Without it a choice
	// that has gone narrows nothing, as the report page's remembered one.
	Exact bool
	// Reader is the reader's user uuid: their standing says which money
	// fields they may slice by (those of projects they are an admin of:
	// money is for project admins only). Empty slices by none.
	Reader string
	// Fields are the field ways ("field:<id>") a widget draws: sliced even
	// past insightFieldsCap.
	Fields []string
}

// ReportFor is the report the reader asked for, the one path the report page
// and a dashboard's widgets take: the filters go through the list's own
// builder, with one project the reader reads its side filters (cycles,
// custom fields, types) too, as on its list; across several, none, as on a
// person's own list. A search that can't be made safe is
// taskDomain.ErrSearchTerm, the asker's to fix.
func ReportFor(ctx context.Context, readerUID string, now time.Time, loc *time.Location, a ReportAsk) (*Report, error) {
	project := ""
	if len(a.Only) == 1 && (len(a.Params) > 0 || a.Search != "") {
		// A side filter reads the project's own cycles and fields: only for
		// a reader of it, as its list checks first.
		if p, err := GetBasicDgraphProjectInfo(ctx, strings.TrimSpace(a.Only[0]), readerUID); err == nil && p.ReaderIsIn() {
			project = p.Uuid
		}
	}
	filter, err := ListFilter(ctx, project, a.Params, a.Search, now).Clause()
	if err != nil {
		return nil, err
	}
	return getReport(ctx, readerUID, now, loc, filter, a)
}

// getReport reads and works out the report of the reader's live projects
// (only those in a.Only, if any) for a.Weeks up to now's, in loc, of the
// tasks filter matches ("" for all: a view's filters, ListFilter's Clause).
// The hours are left out, not the report, if the time logged can't be
// read; the custom-field slices, if the fields can't be; and a status's,
// cycle's or scale's names and keys, if those can't be. Each is one read for
// all the report's projects. With a.Exact, projects asked for that are none
// of the reader's keep none.
func getReport(ctx context.Context, readerUID string, now time.Time, loc *time.Location, filter string, a ReportAsk) (*Report, error) {
	weeks := a.Weeks
	if weeks < 1 || weeks > MaxReportWeeks {
		weeks = ReportWeeks
	}
	first := weekStart(now, loc).AddDate(0, 0, -7*(weeks-1))
	projects, err := domain.GetDgraphReport(ctx, readerUID, first, filter)
	if err != nil {
		return nil, err
	}
	keep, none := narrowFor(projects, a.Only, a.Exact)
	if none {
		// None of the projects asked for is the reader's: an empty report,
		// never the reader's others.
		projects = nil
	}
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
	own, statuses := ownStatuses(ctx, ids)
	r.Flow = buildFlow(projects, own, first, now, weeks, keep)
	// The insights slice by type: the tasks' types live in Postgres.
	var tasks []*dgraphStruct.DgraphTask
	for _, p := range projects {
		if len(keep) == 0 || keep[p.UUID] {
			tasks = append(append(tasks, p.Open...), p.Closed...)
		}
	}
	taskKeys.Lay(ctx, tasks...)
	in := insightIn{own: own, first: first, now: now, today: dayStart(now, loc), only: keep, statuses: statuses,
		cycles: reportCycles(ctx, ids), scales: reportScales(ctx, ids)}
	var capped bool
	in.fields, capped = reportFields(ctx, readerUID, a, ids, projects, tasks)
	r.Insights = buildInsights(projects, in)
	r.Insights.FieldsCapped = capped
	return r, nil
}

// reportCycles is the live cycle of each task in one, of the projects, by
// task uuid: one read. A failed read costs the cycle slices' names, not the
// report: every task reads as in none.
func reportCycles(ctx context.Context, projects []uuid.UUID) map[string]cycleModel.Cycle {
	cycles, err := cycleModel.ByTaskIn(ctx, projects)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetReport cycles err: %+v", err)
	}
	return cycles
}

// reportScales is the estimate scale of each of the projects that has one,
// by project uuid: one read. A failed read reads them all as hours.
func reportScales(ctx context.Context, projects []uuid.UUID) map[string]*estimateScale.Scale {
	out := map[string]*estimateScale.Scale{}
	scales, err := estimateScale.OfMany(ctx, projects)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetReport scales err: %+v", err)
	}
	for id, s := range scales {
		out[id.String()] = s
	}
	return out
}

// reportFields are the ways to slice the report's tasks by its projects'
// custom fields (fieldDims), with the tasks' values laid on them, the
// people their person fields name read, and, when a money field is among
// them, the projects of those the reader is an admin of: each in one read.
// Only the projects the report kept are read, all of them the reader's, and
// only the tasks it kept (restricted ones the reader can't read are never
// among them) give a field's values. A failed read costs the fields'
// slices (a money field's: fails closed), not the report.
func reportFields(ctx context.Context, readerUID string, a ReportAsk, projects []uuid.UUID, work []domain.ReportProject, tasks []*dgraphStruct.DgraphTask) ([]insightDim, bool) {
	reader := taskFieldBusiness.Reader{UUID: a.Reader, UID: readerUID}
	fields, err := taskFieldModel.ListForProjects(ctx, projects)
	if err == nil && len(fields) > 0 {
		err = taskFieldBusiness.MergeFieldValuesFor(ctx, reader, tasks)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetReport fields err: %+v", err)
		return nil, false
	}
	ask := fieldAsk{fields: fields, names: map[string]string{}, always: map[string]bool{}}
	for _, by := range a.Fields {
		ask.always[by] = true
	}
	var ids, money []string
	seen, inMoney := map[string]bool{}, map[string]bool{}
	for _, f := range fields {
		switch f.Type {
		case taskFieldModel.TypeMoney:
			if !inMoney[f.ProjectID.String()] {
				inMoney[f.ProjectID.String()] = true
				money = append(money, f.ProjectID.String())
			}
		case taskFieldModel.TypePerson:
			for _, t := range tasks {
				var id string
				if json.Unmarshal(t.Fields[f.ID.String()], &id) == nil && id != "" && !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	if len(money) > 0 {
		admin, err := taskFieldBusiness.MoneyProjects(ctx, reader, money)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/GetReport money admins err: %+v", err)
		}
		ask.admin = admin
	}
	if len(ids) > 0 {
		people, err := userDomain.GetUserDisplayMapByUUIDs(ctx, ids)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/GetReport field people err: %+v", err)
		}
		for id, u := range people {
			if u != nil {
				ask.names[id] = u.DisplayName()
			}
		}
	}
	return fieldDims(ask, work)
}

// ownStatuses is each project's own statuses, lower-cased name → category,
// by project uuid: how a task's history names them; and their names by id,
// as a board names its columns. Without them, a history naming one reads as
// the status before it (flow.go), and a task in one as its category, so a
// failure to read them costs precision, not the report.
func ownStatuses(ctx context.Context, ids []uuid.UUID) (map[string]map[string]string, map[string]string) {
	out, names := map[string]map[string]string{}, map[string]string{}
	byProject, err := taskStatusModels.ListForProjects(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetReport statuses err: %+v", err)
		return out, names
	}
	for id, statuses := range byProject {
		own := map[string]string{}
		for _, st := range statuses {
			own[strings.ToLower(strings.TrimSpace(st.Name))] = st.Category
			names[st.ID.String()] = st.Name
		}
		out[id.String()] = own
	}
	return out, names
}

// dated says a task's date is set: the graph keeps an unset one as its zero.
func dated(t *time.Time) bool { return t != nil && t.Year() > 1970 }
