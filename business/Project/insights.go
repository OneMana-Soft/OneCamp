package business

// Work insights: how long work takes, read from the same status history as
// the flow of work (flow.go). Cycle time is from a task first being started
// to it being done, lead time from it being made to it being done, triage
// time from it being made in the backlog to it leaving it; age is how long
// open work has been open, and time in status how long done work spent at
// each step. Each comes as its spread (the 25th to 95th percentiles), whole
// and sliced by project, person, priority, type, status, labels, cycle,
// estimate and every custom field of the projects (L32), each slice with its
// open, overdue and done work and their effort beside, and each done task as
// a point for a control chart. Linear keeps insights for its Business plan,
// Jira its control chart for its paid ones; this is on every plan.

import (
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// insightPointsCap is how many done tasks a report draws as points, the
// latest first. The spreads count them all.
var insightPointsCap = 500

// Insights is how long the report's work took, in hours. Cycle, lead and
// time in status are the tasks done in the weeks shown; triage the tasks
// that left the backlog in them; age the tasks open now.
type Insights struct {
	Cycle  helpers.Spread `json:"cycle"`
	Lead   helpers.Spread `json:"lead"`
	Triage helpers.Spread `json:"triage"`
	Age    helpers.Spread `json:"age"`
	// Effort is the size of the open work and of the work done in the
	// weeks shown (L32), as each slice's is.
	Effort       InsightEffort   `json:"effort"`
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

// InsightField is a custom field of one of the report's projects the work
// is sliced by: any kind, a money field only for its project's admins.
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
	// In is the task's keys in every way it is sliced by (a built-in way, or
	// "field:<id>"), "" for none: for a chart to pick out any slice (L32).
	// Person, priority and type above stay for the app before it.
	In map[string][]string `json:"in,omitempty"`
}

// Effort is how big work is: its points, where its project estimates in
// them (business/EstimateScale Counted, as burndown and velocity count
// them), and its hours, from its estimates in time. Size, never money.
type Effort struct {
	Points float64 `json:"points"`
	Hours  float64 `json:"hours"`
}

func (e Effort) plus(o Effort) Effort {
	return Effort{Points: e.Points + o.Points, Hours: e.Hours + o.Hours}
}

// InsightEffort is the effort of open work and of the work done in the
// weeks shown, each to the hundredth.
type InsightEffort struct {
	Open Effort `json:"open"`
	Done Effort `json:"done"`
}

func (e InsightEffort) rounded() InsightEffort {
	r := func(x float64) float64 { return math.Round(x*100) / 100 }
	return InsightEffort{Open: Effort{r(e.Open.Points), r(e.Open.Hours)}, Done: Effort{r(e.Done.Points), r(e.Done.Hours)}}
}

// effortOf is a task's effort in a project on scale (nil: hours). Pure.
func effortOf(scale *estimateScale.Scale, t *dgraphStruct.DgraphTask) Effort {
	var e Effort
	if scale != nil {
		e.Points = estimateScale.Counted(scale, t.EstimatePoints)
	}
	if t.EstimateMinutes != nil && *t.EstimateMinutes > 0 {
		e.Hours = float64(*t.EstimateMinutes) / 60
	}
	return e
}

// InsightSlice is the spreads of one way's one key: a project, a person, a
// label, a field's value. An empty key is the work with none: nobody's, no
// priority, a plain task, no label, cycle or estimate. Beside the spreads
// are its open tasks, those of them overdue, its tasks done in the weeks
// shown, and their effort (L32).
type InsightSlice struct {
	By      string         `json:"by"`
	Key     string         `json:"key"`
	Name    string         `json:"name"`
	Cycle   helpers.Spread `json:"cycle"`
	Lead    helpers.Spread `json:"lead"`
	Age     helpers.Spread `json:"age"`
	Open    int            `json:"open"`
	Overdue int            `json:"overdue"`
	Done    int            `json:"done"`
	Effort  InsightEffort  `json:"effort"`
}

// SlicesBy is the slices of one way, in their order; none is empty.
func (in Insights) SlicesBy(by string) []InsightSlice {
	out := []InsightSlice{}
	for _, s := range in.Slices {
		if s.By == by {
			out = append(out, s)
		}
	}
	return out
}

// InsightBys are the built-in ways work is sliced, in order (builtInDims);
// a custom field's is "field:<id>".
var InsightBys = []string{"project", "person", "priority", "type", "status", "labels", "cycle", "estimate"}

// InsightBy is by as a way to slice work, a field's with its id written
// canonically, and whether it is one: what a dashboard's breakdown may be
// by. Pure.
func InsightBy(by string) (string, bool) {
	if slices.Contains(InsightBys, by) {
		return by, true
	}
	if id, ok := strings.CutPrefix(by, "field:"); ok {
		if u, err := uuid.Parse(id); err == nil {
			return "field:" + u.String(), true
		}
	}
	return "", false
}

// insightKey is a task's place in one way of slicing: its key and name.
type insightKey struct{ key, name string }

// insightDim is one way to slice work: each task's keys and names in it
// (one, or several for labels or a multi-select), or none to leave it out
// (an agent's work is nobody's load; another project's task has no value of
// a field). A custom field is one more of these, with field set (fieldDims).
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

// insightIn is what the insights are worked out of besides the projects:
// their own statuses (lower-cased name → category, by project: flow.go),
// the weeks from first to now, the start of the reader's today (overdue is
// due before it), the projects kept (none: all), their own statuses' names
// by id (live ones: a status taken away reads as its category, as on the
// board), each task's live cycle by task uuid, each project's estimate
// scale (none: hours) and the custom fields' ways (fieldDims). Each is one
// read for the whole report (getReport).
type insightIn struct {
	own        map[string]map[string]string
	first, now time.Time
	today      time.Time
	only       map[string]bool
	statuses   map[string]string
	cycles     map[string]cycleModel.Cycle
	scales     map[string]*estimateScale.Scale
	fields     []insightDim
}

// hourBands are the estimates in time an estimate slice groups, by minutes
// under each.
type hourBand struct {
	under     int
	key, name string
}

var hourBands = []hourBand{{60, "0-1h", "Under 1 h"}, {240, "1-4h", "1–4 h"}, {480, "4-8h", "4–8 h"}, {960, "8-16h", "8–16 h"}, {2400, "16-40h", "16–40 h"},
	{math.MaxInt, "40h+", "40 h or more"}}

// builtInDims are the built-in ways to slice work, in InsightBys' order,
// reading in. Pure.
func builtInDims(in insightIn) []insightDim {
	none := one("", "", true)
	return []insightDim{
		{by: "project", of: func(p *domain.ReportProject, _ *dgraphStruct.DgraphTask) []insightKey {
			return one(p.UUID, p.Name, true)
		}},
		{by: "person", of: func(_ *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
			u := t.Assignee
			switch {
			case u != nil && u.IsBot:
				return nil
			case u == nil || u.Uuid == "" || dated(u.DeletedAt):
				return none
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
				return none
			}
			return one(t.WorkType.ID, t.WorkType.Name, true)
		}},
		// A project's own status by its name; else the built-in one.
		{by: "status", of: func(_ *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
			if t.CustomStatus != nil {
				if name, ok := in.statuses[*t.CustomStatus]; ok {
					return one(*t.CustomStatus, name, true)
				}
			}
			return one(t.Status, taskStatusBusiness.Resolved{Category: t.Status}.Display(), true)
		}},
		// Each of a task's labels (helpers.Tags), case folded, named as
		// first written.
		{by: "labels", of: func(_ *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
			var keys []insightKey
			if t.Label != nil {
				for _, tag := range helpers.Tags(*t.Label) {
					k := strings.ToLower(tag)
					if !slices.ContainsFunc(keys, func(x insightKey) bool { return x.key == k }) {
						keys = append(keys, insightKey{k, tag})
					}
				}
			}
			if len(keys) == 0 {
				return none
			}
			return keys
		}},
		{by: "cycle", of: func(p *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
			if c, ok := in.cycles[t.Uuid]; ok && c.ProjectUUID.String() == p.UUID {
				return one(c.Id.String(), c.Label(), true)
			}
			return none
		}},
		// Points as the project's scale names them; in a project without
		// one, hours in bands.
		{by: "estimate", of: func(p *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
			if s := in.scales[p.UUID]; s != nil {
				if t.EstimatePoints == nil {
					return none
				}
				l := estimateScale.Label(s, *t.EstimatePoints)
				return one(l, l, true)
			}
			if t.EstimateMinutes == nil || *t.EstimateMinutes <= 0 {
				return none
			}
			i := slices.IndexFunc(hourBands, func(b hourBand) bool { return *t.EstimateMinutes < b.under })
			return one(hourBands[i].key, hourBands[i].name, true)
		}},
	}
}

// insightFieldsCap is how many custom fields a report slices by.
const insightFieldsCap = 20

// The most values of a text or link field sliced on their own, and how many
// equal ranges a number or money field with more distinct values falls in.
const (
	insightTextsCap   = 12
	insightNumbersCap = 12
	insightRanges     = 6
)

// otherKey is the slice of a text or link field's values past the top
// insightTextsCap: never a value's key, which are case folded.
const otherKey = "Other"

// fieldAsk is what slices by custom fields: the fields, in order; the names
// of the people their person fields name (someone not in it is a former
// member); the projects whose money the reader may read, by id (their
// admins: money is for project admins only); and the field ways
// ("field:<id>") a widget draws, sliced past insightFieldsCap.
type fieldAsk struct {
	fields []*taskFieldModel.Field
	names  map[string]string
	admin  map[string]bool
	always map[string]bool
}

// fieldDims are the ways to slice by projects' custom fields (B9-b1, L32):
// each field of a kind fieldKeys slices, keyed by its value as fieldKeys
// says, read from each task's values (t.Fields). A task of another project
// is in none of a field's slices; one without a value is in its "" slice. A
// money field, and a formula reading one, is left out, and counts nothing
// against the cap, unless the reader is its project's admin. At most insightFieldsCap, besides those
// always asked for, and capped says there were more. projects are what the
// text and number fields' values are read from. Pure.
func fieldDims(a fieldAsk, projects []domain.ReportProject) (dims []insightDim, capped bool) {
	// Money, and every formula reading it, by V-698's one rule (MoneyOf);
	// a formula slices as the kind of value it gives (ResultTypes).
	money, results := taskFieldBusiness.MoneyOf(a.fields), taskFieldBusiness.ResultTypes(a.fields)
	for _, f := range a.fields {
		if f == nil {
			continue
		}
		if p, ok := money[f.ID.String()]; ok && !a.admin[p] {
			continue
		}
		project, id := f.ProjectID.String(), f.ID.String()
		by := "field:" + id
		as := f
		if t, ok := results[id]; ok {
			stand := *f
			stand.Type = t
			as = &stand
		}
		keyOf := fieldKeys(as, a.names, func() []json.RawMessage { return fieldValues(projects, project, id) })
		if keyOf == nil {
			continue
		}
		if len(dims) >= insightFieldsCap && !a.always[by] {
			capped = true
			continue
		}
		dims = append(dims, insightDim{
			by:    by,
			field: &InsightField{By: by, Name: f.Name, Type: f.Type, Project: project},
			of: func(p *domain.ReportProject, t *dgraphStruct.DgraphTask) []insightKey {
				if p.UUID != project {
					return nil
				}
				if keys := keyOf(t.Fields[id]); len(keys) > 0 {
					return keys
				}
				return one("", "", true)
			},
		})
	}
	return dims, capped
}

// fieldValues is the values of field id on project's tasks. Pure.
func fieldValues(projects []domain.ReportProject, project, id string) []json.RawMessage {
	var out []json.RawMessage
	for i := range projects {
		if projects[i].UUID != project {
			continue
		}
		for _, list := range [][]*dgraphStruct.DgraphTask{projects[i].Open, projects[i].Closed} {
			for _, t := range list {
				if t != nil && len(t.Fields[id]) > 0 {
					out = append(out, t.Fields[id])
				}
			}
		}
	}
	return out
}

// fieldKeys is how a value of f is keyed and named, or nil for a kind that
// doesn't slice: a select's or multi-select's options by id and label (one
// taken away since is no slice of its own); a person by id and name; a
// checkbox Yes or No; text and links trimmed and case folded, named as first
// written, the insightTextsCap with most tasks on their own and the rest
// Other; dates by month; numbers and money (in its currency) each value, or
// past insightNumbersCap distinct values, insightRanges equal ranges. The
// kinds with many values read values, the field's values, to choose. Pure.
func fieldKeys(f *taskFieldModel.Field, names map[string]string, values func() []json.RawMessage) func(json.RawMessage) []insightKey {
	text := func(raw json.RawMessage) string {
		var s string
		_ = json.Unmarshal(raw, &s)
		return strings.TrimSpace(s)
	}
	switch f.Type {
	case taskFieldModel.TypeSelect, taskFieldModel.TypeMultiSelect, taskFieldModel.TypePerson:
		labels := map[string]string{}
		for _, o := range f.Options {
			labels[o.ID] = o.Label
		}
		return func(raw json.RawMessage) []insightKey {
			var values []string
			if f.Type == taskFieldModel.TypeMultiSelect {
				_ = json.Unmarshal(raw, &values)
			} else if v := text(raw); v != "" {
				values = []string{v}
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
					keys = append(keys, insightKey{v, name})
				}
			}
			return keys
		}
	case taskFieldModel.TypeCheckbox:
		return func(raw json.RawMessage) []insightKey {
			var b bool
			if json.Unmarshal(raw, &b) == nil && b {
				return one("yes", "Yes", true)
			}
			return one("no", "No", true)
		}
	case taskFieldModel.TypeText, taskFieldModel.TypeURL:
		count, name := map[string]int{}, map[string]string{}
		for _, raw := range values() {
			if v := text(raw); v != "" {
				k := strings.ToLower(v)
				if count[k]++; count[k] == 1 {
					name[k] = v
				}
			}
		}
		top := make([]string, 0, len(count))
		for k := range count {
			top = append(top, k)
		}
		sort.Slice(top, func(i, j int) bool {
			return count[top[i]] > count[top[j]] || (count[top[i]] == count[top[j]] && top[i] < top[j])
		})
		if len(top) > insightTextsCap {
			for _, k := range top[insightTextsCap:] {
				delete(name, k)
			}
		}
		return func(raw json.RawMessage) []insightKey {
			v := text(raw)
			if v == "" {
				return nil
			}
			if n, ok := name[strings.ToLower(v)]; ok {
				return one(strings.ToLower(v), n, true)
			}
			return one(otherKey, "Other", true)
		}
	case taskFieldModel.TypeDate:
		return func(raw json.RawMessage) []insightKey {
			d, err := time.Parse(time.DateOnly, text(raw))
			return one(d.Format("2006-01"), d.Format("Jan 2006"), err == nil)
		}
	case taskFieldModel.TypeNumber, taskFieldModel.TypeMoney:
		return numberKeys(f, values())
	}
	return nil
}

// numberKeys keys a number or money field's values: each value while there
// are at most insightNumbersCap distinct ones, else insightRanges equal
// ranges from the least to the most. Money is in hundredths of its currency
// and named in it. Pure.
func numberKeys(f *taskFieldModel.Field, values []json.RawMessage) func(json.RawMessage) []insightKey {
	money := f.Type == taskFieldModel.TypeMoney
	key := func(x float64) string { return strconv.FormatFloat(math.Round(x*100)/100, 'f', -1, 64) }
	show := func(x float64) string {
		if money {
			return helpers.Money(int64(math.Round(x)))
		}
		return key(x)
	}
	unit := ""
	if money && f.Currency != "" {
		unit = " " + f.Currency
	}
	seen := map[float64]bool{}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, raw := range values {
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			seen[v] = true
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	width := (hi - lo) / insightRanges
	return func(raw json.RawMessage) []insightKey {
		var v float64
		if json.Unmarshal(raw, &v) != nil {
			return nil
		}
		if len(seen) <= insightNumbersCap || !(width > 0) {
			return one(key(v), show(v)+unit, true)
		}
		i := max(0, min(int((v-lo)/width), insightRanges-1))
		a, b := lo+width*float64(i), lo+width*float64(i+1)
		return one(key(a)+".."+key(b), show(a)+"–"+show(b)+unit, true)
	}
}

// steps are the statuses time in status reports, in the order work moves.
var steps = []string{dgraphStruct.TASK_STATUS_BACKLOG, dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_INPROGRESS, dgraphStruct.TASK_STATUS_INREVIEW}

// hours is d in hours, to the hundredth.
func hours(d time.Duration) float64 { return math.Round(d.Hours()*100) / 100 }

// buildInsights is how long the work of projects took, and how big it is,
// sliced every way: the built-in ways and in.fields, as in says. A task
// without a creation date has no lead time or age and is left out of them,
// and of the points; it still counts as open or done. Pure.
func buildInsights(projects []domain.ReportProject, in insightIn) Insights {
	dims := append(builtInDims(in), in.fields...)
	var cycle, lead, triage, age []float64
	var effort InsightEffort
	inStatus := map[string][]float64{}
	points := []InsightPoint{}
	type sliceOf struct {
		InsightSlice
		cycle, lead, age []float64
	}
	byKey := map[string]*sliceOf{}
	var order []string
	// keysOf is a task's keys in each way, in dims' order.
	keysOf := func(p *domain.ReportProject, t *dgraphStruct.DgraphTask) [][]insightKey {
		out := make([][]insightKey, len(dims))
		for i, d := range dims {
			out[i] = d.of(p, t)
		}
		return out
	}
	slice := func(keys [][]insightKey, add func(s *sliceOf)) {
		for i, d := range dims {
			for _, k := range keys[i] {
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
	inWeeks := func(at time.Time) bool { return !at.Before(in.first) && !at.After(in.now) }
	for i := range projects {
		p := &projects[i]
		if len(in.only) > 0 && !in.only[p.UUID] {
			continue
		}
		scale := in.scales[p.UUID]
		for _, t := range p.Open {
			if t == nil {
				continue
			}
			spans := statusSpans(t, in.own[p.UUID])
			if left, ok := triageOf(t, spans); ok && inWeeks(left) {
				triage = append(triage, hours(left.Sub(spans[0].at)))
			}
			e, overdue := effortOf(scale, t), dated(t.DueDate) && t.DueDate.Before(in.today)
			effort.Open = effort.Open.plus(e)
			h, aged := 0.0, dated(t.CreatedAt) && in.now.After(*t.CreatedAt)
			if aged {
				h = hours(in.now.Sub(*t.CreatedAt))
				age = append(age, h)
			}
			slice(keysOf(p, t), func(s *sliceOf) {
				s.Open++
				if overdue {
					s.Overdue++
				}
				s.Effort.Open = s.Effort.Open.plus(e)
				if aged {
					s.age = append(s.age, h)
				}
			})
		}
		for _, t := range p.Closed {
			if t == nil {
				continue
			}
			spans := statusSpans(t, in.own[p.UUID])
			if left, ok := triageOf(t, spans); ok && inWeeks(left) {
				triage = append(triage, hours(left.Sub(spans[0].at)))
			}
			last := spans[len(spans)-1]
			if last.category != dgraphStruct.TASK_STATUS_DONE || !inWeeks(last.at) {
				continue
			}
			e, keys := effortOf(scale, t), keysOf(p, t)
			effort.Done = effort.Done.plus(e)
			if !dated(t.CreatedAt) {
				slice(keys, func(s *sliceOf) { s.Done++; s.Effort.Done = s.Effort.Done.plus(e) })
				continue
			}
			pt := InsightPoint{Task: t.Uuid, Name: t.Name, DoneAt: last.at, Lead: hours(last.at.Sub(*t.CreatedAt)), Project: p.UUID, In: map[string][]string{}}
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
			for i, d := range dims {
				for _, k := range keys[i] {
					pt.In[d.by] = append(pt.In[d.by], k.key)
				}
			}
			firstKey := func(by string) string {
				if keys := pt.In[by]; len(keys) > 0 {
					return keys[0]
				}
				return ""
			}
			pt.Person, pt.Priority, pt.Type = firstKey("person"), firstKey("priority"), firstKey("type")
			lead = append(lead, pt.Lead)
			if pt.Cycle != nil {
				cycle = append(cycle, *pt.Cycle)
			}
			points = append(points, pt)
			slice(keys, func(s *sliceOf) {
				s.Done++
				s.Effort.Done = s.Effort.Done.plus(e)
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
		Effort: effort.rounded(), InStatus: make([]InsightStatus, 0, len(steps)), Points: points, Slices: make([]InsightSlice, 0, len(order)),
		Fields: []InsightField{}}
	for _, d := range in.fields {
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
		s.Cycle, s.Lead, s.Age, s.Effort = helpers.SpreadOf(s.cycle), helpers.SpreadOf(s.lead), helpers.SpreadOf(s.age), s.Effort.rounded()
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
