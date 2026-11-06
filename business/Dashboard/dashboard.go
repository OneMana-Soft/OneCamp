// Package business (Dashboard) is dashboards (B9-b2: jira-08, asana-08,
// linear-32): pages of widgets (metrics, charts and tables) over the report
// and its insights (business/Project ReportFor) and over goals, each with a
// COPY of its query taken when it was added: its projects, a saved view's
// filters and search, its weeks and what it measures.
//
// WHO (the permission model; tests/integration/dashboards_http_test.go has a
// block for each case):
//  1. mine: only its owner lists, reads and keeps it; anyone else finds
//     nothing (404, not 403: no oracle). It is never shared.
//  2. a project's: its admins keep it (Work Access NeedManage), never while
//     the project is archived; its members, commenters and viewers list and
//     read it once it is shared, and its admins before. Anyone the project
//     isn't there for (a team's admin outside it included) finds nothing.
//  3. the workspace's: its admins keep it; every member reads it once shared.
//  4. guests are never users: they hold guest grants and never pass the
//     member sign-in, so every dashboards route answers them 401.
//  5. nothing a widget shows is kept: each read works every widget out as
//     the reader (Compute), so a widget on a project the reader isn't in reads
//     empty, never its maker's numbers. A widget's query is its keeper's:
//     those who keep the dashboard read it whole, to change it; a reader gets
//     only its shape (Shown), never the projects, filters, search or goal it
//     reads, which may name work outside the reader's reach.
//
// API tokens act as their member; no bot keeps dashboards.
package business

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/google/uuid"
)

// The limits: dashboards a scope keeps, widgets a dashboard has, and what one
// widget's query may name.
const (
	MaxDashboards = 50
	MaxWidgets    = 24
	MaxProjects   = 20
	MaxFilters    = 30
	MaxSearch     = 200
	MaxName       = 80
	MaxWeeks      = 52
	Columns       = 12
	MaxRow        = 999
	MaxHeight     = 12
)

// The scopes a dashboard is kept in.
const (
	ScopeMine      = "mine"
	ScopeProject   = "project"
	ScopeWorkspace = "workspace"
)

// The kinds of widget, and what each shows (shape).
const (
	KindMetric       = "metric"         // one number: Measure
	KindThroughput   = "throughput"     // done and added each week
	KindControlChart = "control_chart"  // each task done: its cycle and lead time
	KindTimeInStatus = "time_in_status" // how long done work spent in each step
	KindFlow         = "flow"           // where the work stood at each week's end
	KindBreakdown    = "breakdown"      // a table of open, overdue and done By
	KindGoal         = "goal"           // one goal's progress and health
)

var kinds = map[string]bool{KindMetric: true, KindThroughput: true, KindControlChart: true, KindTimeInStatus: true,
	KindFlow: true, KindBreakdown: true, KindGoal: true}

// measures are what a metric widget may show: a count, or a spread of days.
var measures = map[string]bool{"open": true, "overdue": true, "due_this_week": true, "done_total": true, "on_time": true,
	"cycle": true, "lead": true, "triage": true, "age": true}

// breakdowns are what a breakdown widget's rows may be.
var breakdowns = map[string]bool{"project": true, "person": true, "priority": true}

var (
	// ErrNotFound: no such dashboard or widget here, or not for this reader.
	ErrNotFound = errors.New("that dashboard isn't there")
	// ErrFull: the scope keeps MaxDashboards, or the dashboard MaxWidgets.
	ErrFull = errors.New("there's no room for another here")
)

// Refusal is input a dashboard or widget can't keep, in words for the person.
type Refusal struct{ msg string }

func (r *Refusal) Error() string { return r.msg }

func refuse(format string, a ...any) error { return &Refusal{msg: fmt.Sprintf(format, a...)} }

// Dashboard is one dashboard as it is answered.
type Dashboard struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	ProjectID string    `json:"project_id,omitempty"`
	Name      string    `json:"name"`
	Shared    bool      `json:"shared"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// MineOf is the owner of a person's own: never answered.
	MineOf uuid.UUID `json:"-"`
}

// Query is a widget's own copy of what it reads. Projects empty is all the
// reader's (a project's dashboard: that project alone); Filters and Search
// are a saved view's, as its list sends them.
type Query struct {
	Projects []string                   `json:"projects,omitempty"`
	Filters  []taskDomain.TaskListParam `json:"filters,omitempty"`
	Search   string                     `json:"search,omitempty"`
	Weeks    int                        `json:"weeks,omitempty"`
	Measure  string                     `json:"measure,omitempty"`
	By       string                     `json:"by,omitempty"`
	Goal     string                     `json:"goal,omitempty"`
}

// Widget is one widget, at X, Y on a Columns-wide grid, W wide and H tall.
type Widget struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Query Query  `json:"query"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
}

// Place is where a widget goes.
type Place struct {
	ID string `json:"id"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
	W  int    `json:"w"`
	H  int    `json:"h"`
}

// MayRead says whether a person (who, a workspace admin or not, standing s
// in a project's dashboard's project) may list and read d. Pure.
func MayRead(d Dashboard, who uuid.UUID, s workAccess.Standing, isAdmin bool) bool {
	switch d.Scope {
	case ScopeMine:
		return d.MineOf == who
	case ScopeProject:
		return s.Can(workAccess.NeedView) && (d.Shared || s.Can(workAccess.NeedManage))
	}
	return d.Shared || isAdmin
}

// MayKeep says whether they may change, share, delete or restore d and its
// widgets. Pure.
func MayKeep(d Dashboard, who uuid.UUID, s workAccess.Standing, isAdmin bool) bool {
	switch d.Scope {
	case ScopeMine:
		return d.MineOf == who
	case ScopeProject:
		return !s.Archived && s.Can(workAccess.NeedManage)
	}
	return isAdmin
}

// CheckName is a dashboard's name, trimmed, or why it can't be. Pure.
func CheckName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", refuse("Give the dashboard a name.")
	}
	if utf8.RuneCountInString(name) > MaxName {
		return "", refuse("A dashboard's name is at most %d characters.", MaxName)
	}
	return name, nil
}

// CheckShared refuses sharing a person's own dashboard. Pure.
func CheckShared(d Dashboard, shared bool) error {
	if shared && d.Scope == ScopeMine {
		return refuse("Your own dashboard is yours alone. Make one for the workspace or a project to share it.")
	}
	return nil
}

// CheckPlace refuses a place off the grid. Pure.
func CheckPlace(x, y, w, h int) error {
	if x < 0 || w < 1 || x+w > Columns || y < 0 || y > MaxRow || h < 1 || h > MaxHeight {
		return refuse("A widget sits on the %d-column grid: from column 0, at most %d wide in all, and 1 to %d rows tall.", Columns, Columns, MaxHeight)
	}
	return nil
}

// CheckWidget is w as d keeps it, or why it can't be: a kind there is, what
// the kind asks for, a query within its limits (on a project's dashboard,
// about that project alone) and a place on the grid. Pure.
func CheckWidget(d Dashboard, w Widget) (Widget, error) {
	if !kinds[w.Kind] {
		return w, refuse("There's no widget of kind %q.", w.Kind)
	}
	w.Title = strings.TrimSpace(w.Title)
	if utf8.RuneCountInString(w.Title) > MaxName {
		return w, refuse("A widget's title is at most %d characters.", MaxName)
	}
	if err := CheckPlace(w.X, w.Y, w.W, w.H); err != nil {
		return w, err
	}
	q, err := checkQuery(d, w.Kind, w.Query)
	w.Query = q
	return w, err
}

func checkQuery(d Dashboard, kind string, q Query) (Query, error) {
	seen := map[string]bool{}
	var projects []string
	for _, p := range q.Projects {
		id, err := uuid.Parse(strings.TrimSpace(p))
		if err != nil {
			return q, refuse("A widget names a project that isn't one.")
		}
		if !seen[id.String()] {
			seen[id.String()] = true
			projects = append(projects, id.String())
		}
	}
	if len(projects) > MaxProjects {
		return q, refuse("A widget reads at most %d projects.", MaxProjects)
	}
	if d.Scope == ScopeProject {
		// A project's dashboard is about that project alone.
		if len(projects) > 1 || (len(projects) == 1 && projects[0] != d.ProjectID) {
			return q, refuse("A widget on a project's dashboard reads that project alone.")
		}
		projects = []string{d.ProjectID}
	}
	q.Projects = projects
	if len(q.Filters) > MaxFilters {
		return q, refuse("A widget keeps at most %d filters.", MaxFilters)
	}
	q.Search = strings.TrimSpace(q.Search)
	if utf8.RuneCountInString(q.Search) > MaxSearch {
		return q, refuse("A widget's search is at most %d characters.", MaxSearch)
	}
	if q.Weeks < 0 || q.Weeks > MaxWeeks {
		return q, refuse("A widget reads 1 to %d weeks.", MaxWeeks)
	}
	switch kind {
	case KindMetric:
		if !measures[q.Measure] {
			return q, refuse("A metric widget measures one of: open, overdue, due_this_week, done_total, on_time, cycle, lead, triage or age.")
		}
		q.By, q.Goal = "", ""
	case KindBreakdown:
		if !breakdowns[q.By] {
			return q, refuse("A breakdown widget is by project, person or priority.")
		}
		q.Measure, q.Goal = "", ""
	case KindGoal:
		id, err := uuid.Parse(strings.TrimSpace(q.Goal))
		if err != nil {
			return q, refuse("A goal widget names one goal.")
		}
		return Query{Goal: id.String()}, nil
	default:
		q.Measure, q.By, q.Goal = "", "", ""
	}
	return q, nil
}

// Shown is widgets as someone who keeps their dashboard (keeps) or only
// reads it sees them. A widget's query is its keeper's, copied when it was
// added, and may name projects, a view's filters, a search or a goal the
// reader can't see: a reader gets what draws the widget (its kind, title and
// place, and the query's weeks, measure and by) and nothing else of the
// query, so a field added to Query later stays the keeper's too. Its data is
// the reader's own (Compute). widgets is never changed. Pure.
func Shown(widgets []Widget, keeps bool) []Widget {
	if keeps {
		return widgets
	}
	out := make([]Widget, len(widgets))
	for i, w := range widgets {
		w.Query = Query{Weeks: w.Query.Weeks, Measure: w.Query.Measure, By: w.Query.By}
		out[i] = w
	}
	return out
}

// CheckArrange is places, every one on the grid and each widget once. Pure.
func CheckArrange(places []Place) error {
	if len(places) > MaxWidgets {
		return refuse("A dashboard has at most %d widgets.", MaxWidgets)
	}
	seen := map[string]bool{}
	for _, p := range places {
		if _, err := uuid.Parse(p.ID); err != nil || seen[p.ID] {
			return refuse("Each widget is placed once.")
		}
		seen[p.ID] = true
		if err := CheckPlace(p.X, p.Y, p.W, p.H); err != nil {
			return err
		}
	}
	return nil
}
