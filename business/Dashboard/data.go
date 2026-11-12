package business

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	goalBusiness "github.com/akashc777/OneCamp/business/Goal"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// Reader is who a dashboard is worked out for.
type Reader struct {
	UUID      uuid.UUID
	DgraphUID string
	IsAdmin   bool
}

// Data is one widget worked out: what it shows (shape's), or why it can't
// show it just now. One widget's failure is that widget's, never the page's.
type Data struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// View is the name of the saved view the widget reads, as the reader
	// reads it: only one they may read is named.
	View  string `json:"view,omitempty"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// readers is how many reports one dashboard reads at once.
const readers = 3

// Compute works every widget out as r, now, in loc. A widget over a saved
// view asks the view's question, read as r (view.go). Widgets that ask the
// same question (projects, filters, search, weeks) share one report: the
// report page's own path (projectBusiness.ReportFor), Exact, so a project r
// isn't in reads as nothing. The reports run side by side (EachBounded: each
// question is its own graph read, with no read of many). Goal widgets read
// the goals once, as r, as the goals page does. Nothing is kept.
func Compute(ctx context.Context, r Reader, now time.Time, loc *time.Location, widgets []Widget) []Data {
	out := make([]Data, len(widgets))
	type ask struct {
		q       Query
		widgets []int
		// fields are the custom fields its breakdowns are by: sliced past
		// the report's cap on fields.
		fields []string
	}
	byKey := map[string]*ask{}
	var asks []*ask
	var goals []int
	views, viewErr := readViews(ctx, r, widgets)
	if viewErr != nil {
		helpers.LogErrorWithContext(ctx, "business/Dashboard.Compute views err: %+v", viewErr)
	}
	for i, w := range widgets {
		out[i] = Data{ID: w.ID, Kind: w.Kind}
		if w.Kind == KindGoal {
			goals = append(goals, i)
			continue
		}
		q := w.Query
		if q.View != "" {
			if viewErr != nil {
				out[i].Error = "This widget couldn't load just now."
				continue
			}
			var why string
			if q, why, out[i].View = fromView(q, views[q.View], r.DgraphUID); why != "" {
				out[i].Error = why
				continue
			}
		}
		k := askKey(q)
		if byKey[k] == nil {
			byKey[k] = &ask{q: q}
			asks = append(asks, byKey[k])
		}
		byKey[k].widgets = append(byKey[k].widgets, i)
		if w.Kind == KindBreakdown && strings.HasPrefix(w.Query.By, "field:") {
			byKey[k].fields = append(byKey[k].fields, w.Query.By)
		}
	}
	if len(goals) > 0 {
		goalData(ctx, r, now, widgets, goals, out)
	}
	helpers.EachBounded("dashboard.reports", asks, readers, func(a *ask) {
		rep, err := projectBusiness.ReportFor(ctx, r.DgraphUID, now, loc, projectBusiness.ReportAsk{
			Weeks: a.q.Weeks, Only: a.q.Projects, Params: a.q.Filters, Search: a.q.Search, Exact: true, Reader: r.UUID.String(), Fields: a.fields})
		if err != nil && !errors.Is(err, taskDomain.ErrSearchTerm) {
			helpers.LogErrorWithContext(ctx, "business/Dashboard.Compute report err: %+v", err)
		}
		for _, i := range a.widgets {
			switch {
			case errors.Is(err, taskDomain.ErrSearchTerm):
				out[i].Error = "This widget's search can't be read. Change its search."
			case err != nil:
				out[i].Error = "This widget couldn't load just now."
			default:
				out[i].Data = shape(widgets[i], rep)
			}
		}
	})
	return out
}

// askKey is the question a report widget asks: two that ask the same share
// one report. A widget over a view asks its view's question (fromView). Pure.
func askKey(q Query) string {
	projects := append([]string(nil), q.Projects...)
	sort.Strings(projects)
	filters, _ := json.Marshal(q.Filters)
	return strings.Join(projects, ",") + "|" + string(filters) + "|" + q.Search + "|" + strconv.Itoa(q.Weeks)
}

// goalData fills the goal widgets at idx from one read of the goals as r: a
// goal that has gone (or never was) says so.
func goalData(ctx context.Context, r Reader, now time.Time, widgets []Widget, idx []int, out []Data) {
	list, err := goalBusiness.List(ctx, goalBusiness.Reader{UUID: r.UUID, DgraphUID: r.DgraphUID, IsAdmin: r.IsAdmin}, now)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Dashboard.Compute goals err: %+v", err)
	}
	byID := make(map[string]goalBusiness.Summary, len(list))
	for _, g := range list {
		byID[g.Id] = g
	}
	for _, i := range idx {
		g, has := byID[widgets[i].Query.Goal]
		switch {
		case err != nil:
			out[i].Error = "This widget couldn't load just now."
		case !has:
			out[i].Error = "That goal isn't there any more."
		default:
			out[i].Data = g
		}
	}
}

// Metric is a metric widget's number: a count, the share done on time, the
// median of a spread of days (with the spread), or an effort (with it: its
// points when it has any, else its hours). Value is nil when there is
// nothing to measure yet.
type Metric struct {
	Measure string                  `json:"measure"`
	Value   *float64                `json:"value"`
	Spread  *helpers.Spread         `json:"spread,omitempty"`
	Effort  *projectBusiness.Effort `json:"effort,omitempty"`
}

// shape is what w shows of the report rep, by its kind. Pure.
func shape(w Widget, rep *projectBusiness.Report) any {
	switch w.Kind {
	case KindMetric:
		return metric(w.Query.Measure, rep)
	case KindThroughput:
		return map[string]any{"weeks": rep.Weeks, "done": rep.Done, "added": rep.Added}
	case KindControlChart:
		return map[string]any{"points": rep.Insights.Points, "points_capped": rep.Insights.PointsCapped,
			"cycle": rep.Insights.Cycle, "lead": rep.Insights.Lead}
	case KindTimeInStatus:
		return map[string]any{"in_status": rep.Insights.InStatus}
	case KindFlow:
		return map[string]any{"weeks": rep.Weeks, "flow": rep.Flow}
	case KindBreakdown:
		// Any way the insights slice work, each slice with its counts and
		// effort; and, by project, person or priority, the rows as before
		// (L32 keeps them for the app before it).
		measure := w.Query.Measure
		if measure == "" {
			measure = "count"
		}
		out := map[string]any{"by": w.Query.By, "measure": measure, "slices": rep.Insights.SlicesBy(w.Query.By)}
		switch w.Query.By {
		case "project":
			out["rows"] = rep.Projects
		case "person":
			out["rows"] = rep.People
		case "priority":
			out["rows"] = rep.Priorities
		}
		for _, f := range rep.Insights.Fields {
			if f.By == w.Query.By {
				out["field"] = f
			}
		}
		return out
	}
	return nil
}

func metric(measure string, rep *projectBusiness.Report) Metric {
	m := Metric{Measure: measure}
	count := func(n int) *float64 { v := float64(n); return &v }
	var s *helpers.Spread
	switch measure {
	case "open":
		m.Value = count(rep.Open)
	case "overdue":
		m.Value = count(rep.Overdue)
	case "due_this_week":
		m.Value = count(rep.DueThisWeek)
	case "done_total":
		m.Value = count(rep.DoneTotal)
	case "on_time":
		if rep.OnTime != nil {
			m.Value = count(*rep.OnTime)
		}
	case "cycle":
		s = &rep.Insights.Cycle
	case "lead":
		s = &rep.Insights.Lead
	case "triage":
		s = &rep.Insights.Triage
	case "age":
		s = &rep.Insights.Age
	case "effort_open", "effort_done":
		e := rep.Insights.Effort.Open
		if measure == "effort_done" {
			e = rep.Insights.Effort.Done
		}
		v := e.Hours
		if e.Points > 0 {
			v = e.Points
		}
		m.Value, m.Effort = &v, &e
	}
	if s != nil {
		spread := *s
		m.Spread = &spread
		if spread.N > 0 {
			v := spread.P50
			m.Value = &v
		}
	}
	return m
}
