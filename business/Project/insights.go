package business

// Work insights: how long work takes, read from the same status history as
// the flow of work (flow.go). Cycle time is from a task first being started
// to it being done, lead time from it being made to it being done, triage
// time from it being made in the backlog to it leaving it; age is how long
// open work has been open, and time in status how long done work spent at
// each step. Each comes as its spread (the 25th to 95th percentiles), whole
// and sliced by project, person, priority, type and the projects' select,
// multi-select and person fields, with each done task as a point for a
// control chart. Linear keeps insights for its Business plan, Jira its
// control chart for its paid ones; this is on every plan.

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
)

// insightPointsCap is how many done tasks a report draws as points, the
// latest first. The spreads count them all.
var insightPointsCap = 500

// Insights is how long the report's work took, in hours. Cycle, lead and
// time in status are the tasks done in the weeks shown; triage the tasks
// that left the backlog in them; age the tasks open now.
type Insights struct {
	Cycle        helpers.Spread  `json:"cycle"`
	Lead         helpers.Spread  `json:"lead"`
	Triage       helpers.Spread  `json:"triage"`
	Age          helpers.Spread  `json:"age"`
	InStatus     []InsightStatus `json:"in_status"`
	Points       []InsightPoint  `json:"points"`
	PointsCapped bool            `json:"points_capped"`
	Slices       []InsightSlice  `json:"slices"`
	// Fields are the custom fields the work is also sliced by, in order
	// (B9-b1): their slices are by "field:<id>".
	Fields []InsightField `json:"fields"`
	// FieldsCapped says the report's projects have more fields to slice
	// by than insightFieldsCap: narrowing to a project slices by its own.
	FieldsCapped bool `json:"fields_capped,omitempty"`
}

// InsightField is a custom field the work is sliced by: a select, a
// multi-select or a person field of one of the report's projects.
type InsightField struct {
	By      string `json:"by"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Project string `json:"project_uuid"`
}

// InsightStatus is how long done tasks spent in one step (a built-in status,
// or the category of a project's own), counting those that spent any.
type InsightStatus struct {
	Status string `json:"status"`
	helpers.Spread
}

// InsightPoint is one task done in the weeks shown: when, how long it took
// (cycle is nil when it was never started), and what it is sliced by.
type InsightPoint struct {
	Task     string    `json:"task_uuid"`
	Name     string    `json:"task_name"`
	DoneAt   time.Time `json:"done_at"`
	Cycle    *float64  `json:"cycle,omitempty"`
	Lead     float64   `json:"lead"`
	Project  string    `json:"project_uuid"`
	Person   string    `json:"person_uuid,omitempty"`
	Priority string    `json:"priority,omitempty"`
	Type     string    `json:"type_id,omitempty"`
	// In is the task's keys in each field it is sliced by ("field:<id>"),
	// "" for none: for a chart to pick out a field's slice.
	In map[string][]string `json:"in,omitempty"`
}

// InsightSlice is the spreads of one project, person, priority or type. An
// empty key is the work with none: nobody's, no priority, a plain task.
type InsightSlice struct {
	By    string         `json:"by"`
	Key   string         `json:"key"`
	Name  string         `json:"name"`
	Cycle helpers.Spread `json:"cycle"`
	Lead  helpers.Spread `json:"lead"`
	Age   helpers.Spread `json:"age"`
}

// insightKey is a task's place in one way of slicing: its key and name.
type insightKey struct{ key, name string }

// insightDim is one way to slice work: each task's keys and names in it
// (one, or several for a multi-select), or none to leave it out (an agent's
// work is nobody's load; another project's task has no value of a field).
// A custom field is one more of these, with field set (fieldDims).
type insightDim struct {
	by    string
	of    func(p *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey
	field *InsightField
}

// one is a single key, or none when ok is false.
func one(key, name string, ok bool) []insightKey {
	if !ok {
		return nil
	}
	return []insightKey{{key, name}}
}

var insightDims = []insightDim{
	{by: "project", of: func(p *domain.ReportProject, _ *dgraphStruct.DgraphTask) []insightKey {
		return one(p.UUID, p.Name, true)
	}},
	{by: "person", of: func(_ *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
		u := t.Assignee
		switch {
		case u != nil && u.IsBot:
			return nil
		case u == nil || u.Uuid == "" || dated(u.DeletedAt):
			return one("", "", true)
		case u.UserFullName != "":
			return one(u.Uuid, u.UserFullName, true)
		}
		return one(u.Uuid, u.UserName, true)
	}},
	{by: "priority", of: func(_ *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
		prio := strings.ToLower(t.Priority)
		if !taskPriority.Is(prio) {
			prio = ""
		}
		return one(prio, prio, true)
	}},
	{by: "type", of: func(_ *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
		if t.WorkType == nil {
			return one("", "", true)
		}
		return one(t.WorkType.ID, t.WorkType.Name, true)
	}},
}

// insightFieldsCap is how many custom fields a report slices by.
const insightFieldsCap = 20

// fieldDims are the ways to slice by projects' custom fields (B9-b1): each
// select, multi-select and person field, in the order given, keyed by its
// option ids or person ids, read from each task's values (t.Fields) and
// named by the option's label or the person's name (names; someone not in
// it is a former member). A task of another project is in none of a field's
// slices; one without a value is in its "" slice. At most insightFieldsCap,
// and capped says there were more. Pure.
func fieldDims(fields []*taskFieldModel.Field, names map[string]string) (dims []insightDim, capped bool) {
	for _, f := range fields {
		if f == nil || (f.Type != taskFieldModel.TypeSelect && f.Type != taskFieldModel.TypeMultiSelect && f.Type != taskFieldModel.TypePerson) {
			continue
		}
		if len(dims) == insightFieldsCap {
			return dims, true
		}
		f := f
		labels := map[string]string{}
		for _, o := range f.Options {
			labels[o.ID] = o.Label
		}
		project, id := f.ProjectID.String(), f.ID.String()
		dims = append(dims, insightDim{
			by:    "field:" + id,
			field: &InsightField{By: "field:" + id, Name: f.Name, Type: f.Type, Project: project},
			of: func(p *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
				if p.UUID != project {
					return nil
				}
				var values []string
				raw := t.Fields[id]
				if f.Type == taskFieldModel.TypeMultiSelect {
					_ = json.Unmarshal(raw, &values)
				} else {
					var v string
					if json.Unmarshal(raw, &v) == nil && v != "" {
						values = []string{v}
					}
				}
				var keys []insightKey
				for _, v := range values {
					switch name, ok := labels[v]; {
					case f.Type == taskFieldModel.TypePerson:
						if name = names[v]; name == "" {
							name = "Former member"
						}
						keys = append(keys, insightKey{v, name})
					case ok:
						// An option taken away since is no slice of its own.
						keys = append(keys, insightKey{v, name})
					}
				}
				if len(keys) == 0 {
					return one("", "", true)
				}
				return keys
			},
		})
	}
	return dims, false
}

// steps are the statuses time in status reports, in the order work moves.
var steps = []string{dgraphStruct.TASK_STATUS_BACKLOG, dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_INPROGRESS, dgraphStruct.TASK_STATUS_INREVIEW}

// hours is d in hours, to the hundredth.
func hours(d time.Duration) float64 { return math.Round(d.Hours()*100) / 100 }

// buildInsights is how long the work of projects took (own: their statuses
// by project uuid; only: the projects kept, when not empty), for the weeks
// from first to now. A task without a creation date has no lead time or age
// and is left out of them. Pure.
func buildInsights(projects []domain.ReportProject, own map[string]map[string]string, first, now time.Time, only map[string]bool, fields ...insightDim) Insights {
	dims := append(insightDims[:len(insightDims):len(insightDims)], fields...)
	var cycle, lead, triage, age []float64
	inStatus := map[string][]float64{}
	points := []InsightPoint{}
	type sliceOf struct {
		InsightSlice
		cycle, lead, age []float64
	}
	byKey := map[string]*sliceOf{}
	var order []string
	slice := func(p *domain.ReportProject, t *dgraphStruct.DgraphTask, add func(s *sliceOf)) {
		for _, d := range dims {
			for _, k := range d.of(p, t) {
				id := d.by + "\x00" + k.key
				s := byKey[id]
				if s == nil {
					s = &sliceOf{InsightSlice: InsightSlice{By: d.by, Key: k.key, Name: k.name}}
					byKey[id] = s
					order = append(order, id)
				}
				add(s)
			}
		}
	}
	inWeeks := func(at time.Time) bool { return !at.Before(first) && !at.After(now) }
	for i := range projects {
		p := &projects[i]
		if len(only) > 0 && !only[p.UUID] {
			continue
		}
		for _, t := range p.Open {
			if t == nil {
				continue
			}
			spans := statusSpans(t, own[p.UUID])
			if left, ok := triageOf(t, spans); ok && inWeeks(left) {
				triage = append(triage, hours(left.Sub(spans[0].at)))
			}
			if dated(t.CreatedAt) && now.After(*t.CreatedAt) {
				h := hours(now.Sub(*t.CreatedAt))
				age = append(age, h)
				slice(p, t, func(s *sliceOf) { s.age = append(s.age, h) })
			}
		}
		for _, t := range p.Closed {
			if t == nil {
				continue
			}
			spans := statusSpans(t, own[p.UUID])
			if left, ok := triageOf(t, spans); ok && inWeeks(left) {
				triage = append(triage, hours(left.Sub(spans[0].at)))
			}
			last := spans[len(spans)-1]
			if last.category != dgraphStruct.TASK_STATUS_DONE || !inWeeks(last.at) || !dated(t.CreatedAt) {
				continue
			}
			pt := InsightPoint{Task: t.Uuid, Name: t.Name, DoneAt: last.at, Lead: hours(last.at.Sub(*t.CreatedAt)), Project: p.UUID}
			for _, s := range spans {
				if s.category == dgraphStruct.TASK_STATUS_INPROGRESS || s.category == dgraphStruct.TASK_STATUS_INREVIEW {
					c := hours(last.at.Sub(s.at))
					pt.Cycle = &c
					break
				}
			}
			spent := map[string]time.Duration{}
			for j, s := range spans[:len(spans)-1] {
				spent[s.category] += spans[j+1].at.Sub(s.at)
			}
			for _, step := range steps {
				if spent[step] > 0 {
					inStatus[step] = append(inStatus[step], hours(spent[step]))
				}
			}
			for _, d := range dims[1:] {
				keys := d.of(p, t)
				key := ""
				if len(keys) > 0 {
					key = keys[0].key
				}
				switch {
				case d.by == "person":
					pt.Person = key
				case d.by == "priority":
					pt.Priority = key
				case d.by == "type":
					pt.Type = key
				case d.field != nil && len(keys) > 0:
					if pt.In == nil {
						pt.In = map[string][]string{}
					}
					for _, k := range keys {
						pt.In[d.by] = append(pt.In[d.by], k.key)
					}
				}
			}
			lead = append(lead, pt.Lead)
			if pt.Cycle != nil {
				cycle = append(cycle, *pt.Cycle)
			}
			points = append(points, pt)
			slice(p, t, func(s *sliceOf) {
				s.lead = append(s.lead, pt.Lead)
				if pt.Cycle != nil {
					s.cycle = append(s.cycle, *pt.Cycle)
				}
			})
		}
	}
	sort.SliceStable(points, func(i, j int) bool {
		if !points[i].DoneAt.Equal(points[j].DoneAt) {
			return points[i].DoneAt.After(points[j].DoneAt)
		}
		return points[i].Task < points[j].Task
	})
	out := Insights{Cycle: helpers.SpreadOf(cycle), Lead: helpers.SpreadOf(lead), Triage: helpers.SpreadOf(triage), Age: helpers.SpreadOf(age),
		InStatus: make([]InsightStatus, 0, len(steps)), Points: points, Slices: make([]InsightSlice, 0, len(order)), Fields: []InsightField{}}
	for _, d := range fields {
		if d.field != nil {
			out.Fields = append(out.Fields, *d.field)
		}
	}
	if len(points) > insightPointsCap {
		out.Points, out.PointsCapped = points[:insightPointsCap], true
	}
	for _, step := range steps {
		out.InStatus = append(out.InStatus, InsightStatus{Status: step, Spread: helpers.SpreadOf(inStatus[step])})
	}
	for _, id := range order {
		s := byKey[id]
		s.Cycle, s.Lead, s.Age = helpers.SpreadOf(s.cycle), helpers.SpreadOf(s.lead), helpers.SpreadOf(s.age)
		out.Slices = append(out.Slices, s.InsightSlice)
	}
	return out
}

// triageOf is when a task made in the backlog first left it; false if it
// wasn't made there, hasn't left, or has no creation date.
func triageOf(t *dgraphStruct.DgraphTask, spans []statusSpan) (time.Time, bool) {
	if !dated(t.CreatedAt) || len(spans) < 2 || spans[0].category != dgraphStruct.TASK_STATUS_BACKLOG {
		return time.Time{}, false
	}
	for _, s := range spans[1:] {
		if s.category != dgraphStruct.TASK_STATUS_BACKLOG {
			return s.at, true
		}
	}
	return time.Time{}, false
}
