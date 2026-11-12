package business

// Formula fields (J-F, jira-01): a task field worked out from the task's
// other fields and its own facts (Builtins) on each read, by the Formula
// Engine tables use (business/Formula), never stored. The field keeps its
// formula with the fields it reads named by id, so a rename doesn't break
// it; the app is given it by name (View). Money is for project admins only,
// and so is every formula that reads it, directly or through other formulas:
// moneyOf counts them with the money fields, so V-698's one rule (Hidden,
// MergeFieldValuesFor) keeps their values from everyone else alike.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/topo"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// TypeFormula is a field worked out from the task's other fields.
const TypeFormula = model.TypeFormula

// Builtin is one of a task's own facts a formula reads by name, as a field:
// {Estimate} (hours), {Points}, {Start date}, {Due date}, {Status},
// {Priority}, {Created}. Its id is a word, never a field's uuid; a custom
// field of the same name is read in its place.
type Builtin struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Result string `json:"result"`
	kind   formula.Kind
	read   func(t *dgraphStruct.DgraphTask, loc *time.Location) formula.Value
}

// day is a date as the day it falls on in the reader's zone.
func day(t *time.Time, loc *time.Location) formula.Value {
	if t == nil || t.Year() <= 1970 {
		return formula.Blank()
	}
	return formula.Date(t.In(loc), true)
}

var builtins = []Builtin{
	{ID: "estimate", Name: "Estimate", kind: formula.KindNumber, read: func(t *dgraphStruct.DgraphTask, _ *time.Location) formula.Value {
		if t.EstimateMinutes == nil || *t.EstimateMinutes <= 0 {
			return formula.Blank()
		}
		return formula.Number(float64(*t.EstimateMinutes) / 60)
	}},
	{ID: "points", Name: "Points", kind: formula.KindNumber, read: func(t *dgraphStruct.DgraphTask, _ *time.Location) formula.Value {
		if t.EstimatePoints == nil {
			return formula.Blank()
		}
		return formula.Number(*t.EstimatePoints)
	}},
	{ID: "start", Name: "Start date", kind: formula.KindDate, read: func(t *dgraphStruct.DgraphTask, loc *time.Location) formula.Value {
		return day(t.StartDate, loc)
	}},
	{ID: "due", Name: "Due date", kind: formula.KindDate, read: func(t *dgraphStruct.DgraphTask, loc *time.Location) formula.Value {
		return day(t.DueDate, loc)
	}},
	{ID: "status", Name: "Status", kind: formula.KindText, read: func(t *dgraphStruct.DgraphTask, _ *time.Location) formula.Value {
		return formula.Text(taskStatusBusiness.Of(t).Display())
	}},
	{ID: "priority", Name: "Priority", kind: formula.KindText, read: func(t *dgraphStruct.DgraphTask, _ *time.Location) formula.Value {
		return formula.Text(helpers.PriorityLabel(t.Priority))
	}},
	{ID: "created", Name: "Created", kind: formula.KindDate, read: func(t *dgraphStruct.DgraphTask, _ *time.Location) formula.Value {
		if t.CreatedAt == nil || t.CreatedAt.Year() <= 1970 {
			return formula.Blank()
		}
		return formula.Date(*t.CreatedAt, false)
	}},
}

var builtinByID = func() map[string]Builtin {
	m := make(map[string]Builtin, len(builtins))
	for i := range builtins {
		builtins[i].Result = builtins[i].kind.Name()
		m[builtins[i].ID] = builtins[i]
	}
	return m
}()

// Builtins is the task's own facts a formula may read, for the app's editor.
func Builtins() []Builtin { return builtins }

// inputs is fields as a formula reads them: the project's fields first, so
// one named like a built-in is read in its place, then the built-ins.
func inputs(fields []*model.Field) []formula.Field {
	out := make([]formula.Field, 0, len(fields)+len(builtins))
	for _, f := range fields {
		if f != nil {
			out = append(out, formula.Field{ID: f.ID.String(), Name: f.Name, Kind: fieldTypes.FormulaKindOf(f.Type),
				IsFormula: f.Type == TypeFormula, Formula: f.Formula})
		}
	}
	for _, b := range builtins {
		out = append(out, formula.Field{ID: b.ID, Name: b.Name, Kind: b.kind})
	}
	return out
}

// hasFormula is whether any of fields is a formula.
func hasFormula(fields []*model.Field) bool {
	for _, f := range fields {
		if f != nil && f.Type == TypeFormula {
			return true
		}
	}
	return false
}

// draftID stands in for a formula field not yet made.
const draftID = "draft"

// compileFormula checks src as the formula of a field of project (fields)
// named name (self: uuid.Nil for one being made): what it gives and the
// formula as stored, or why it can't be read, in the Formula's own words.
func compileFormula(fields []*model.Field, self uuid.UUID, name, src string) (formula.Kind, string, error) {
	id := draftID
	if self != uuid.Nil {
		id = self.String()
	}
	others := make([]*model.Field, 0, len(fields))
	for _, f := range fields {
		if f != nil && f.ID != self {
			others = append(others, f)
		}
	}
	in := inputs(others)
	all := make([]formula.Field, 0, len(in)+1)
	all = append(all, in[:len(others)]...)
	all = append(all, formula.Field{ID: id, Name: name, IsFormula: true, Formula: src})
	all = append(all, in[len(others):]...)
	kind, err := formula.Check(src, all, id)
	if err != nil {
		return kind, "", invalid("%s", strings.TrimSuffix(formula.Message(err), "."))
	}
	stored, err := formula.Canonical(src, all)
	if err != nil {
		return kind, "", invalid("%s", strings.TrimSuffix(formula.Message(err), "."))
	}
	return kind, stored, nil
}

// Preview checks a formula being written for a field of project (fieldID,
// or uuid.Nil for a new one) without saving it: what it gives, or why it
// can't be read. Its values on tasks show once it's saved (none here).
func Preview(ctx context.Context, project, fieldID uuid.UUID, src string) (*formula.Preview, error) {
	fields, err := model.List(ctx, project)
	if err != nil {
		return nil, err
	}
	name := ""
	for _, f := range fields {
		if f.ID == fieldID {
			name = f.Name
		}
	}
	kind, _, err := compileFormula(fields, fieldID, name, src)
	out := &formula.Preview{Result: kind.Name(), Values: []interface{}{}}
	if err != nil {
		out.Error = err.Error()
	}
	return out, nil
}

// tainted is which of fields (by id) give money away, with each one's
// project: the money fields, and every formula that reads one, directly or
// through other formulas (formula.Program.Tainted). Pure.
func tainted(fields []*model.Field) map[string]string {
	money := map[string]string{}
	byProject := map[uuid.UUID][]*model.Field{}
	for _, f := range fields {
		if f == nil {
			continue
		}
		if f.Type == TypeMoney {
			money[f.ID.String()] = f.ProjectID.String()
		}
		byProject[f.ProjectID] = append(byProject[f.ProjectID], f)
	}
	for project, fs := range byProject {
		if !hasFormula(fs) {
			continue
		}
		hidden := map[string]bool{}
		for _, f := range fs {
			if f.Type == TypeMoney {
				hidden[f.ID.String()] = true
			}
		}
		if len(hidden) == 0 {
			continue
		}
		for id := range formula.Compile(inputs(fs)).Tainted(hidden) {
			money[id] = project.String()
		}
	}
	return money
}

// FormulasRead is whether a formula of f's project reads f, directly or
// through other formulas: a change of f's value changes theirs, so the apps
// read the task again (refetch).
func FormulasRead(ctx context.Context, f *model.Field) bool {
	if f == nil || f.Type == TypeFormula {
		return false
	}
	fields, err := model.List(ctx, f.ProjectID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/TaskField FormulasRead err: %+v", err)
		return true // read again rather than show a stale value
	}
	if !hasFormula(fields) {
		return false
	}
	return len(formula.Compile(inputs(fields)).Tainted(map[string]bool{f.ID.String(): true})) > 1
}

// InputsOf is a project's fields as a template keeps them (InputOf), each
// formula with its fields by name, so it reads the same in another project.
func InputsOf(fields []*model.Field) []Input {
	in := inputs(fields)
	out := make([]Input, 0, len(fields))
	for _, f := range fields {
		i := InputOf(f)
		if f.Type == TypeFormula {
			i.Formula = formula.Display(f.Formula, in)
		}
		out = append(out, i)
	}
	return out
}

// CreationOrder is fields in an order they can be made in, one by one: the
// fields that aren't formulas first, then each formula after those it reads
// by name, so none names a field not made yet. Pure.
func CreationOrder(fields []Input) []Input {
	out := make([]Input, 0, len(fields))
	byName := map[string]int{}
	var ids []string
	for i, f := range fields {
		if f.Type != TypeFormula {
			out = append(out, f)
			continue
		}
		byName[strings.ToLower(cleanName(f.Name))] = i
		ids = append(ids, strings.ToLower(cleanName(f.Name)))
	}
	ordered, looped := topo.Order(ids, func(id string) []string {
		var deps []string
		for _, ref := range formula.Refs(fields[byName[id]].Formula) {
			if _, ok := byName[strings.ToLower(strings.TrimSpace(ref))]; ok {
				deps = append(deps, strings.ToLower(strings.TrimSpace(ref)))
			}
		}
		return deps
	})
	for _, id := range append(ordered, looped...) {
		out = append(out, fields[byName[id]])
	}
	return out
}

// formulaRead is one read's formulas: each project's fields and Program,
// all drawing on one budget, worked out for the reader's day (loc).
type formulaRead struct {
	fields   map[string][]*model.Field
	byID     map[string]*model.Field
	programs map[string]*formula.Program
	budget   *formula.Budget
	now      time.Time
	loc      *time.Location
	names    map[string]string
}

// workOut lays each task's formula values among its values (task id, then
// field id), for the tasks of projects (task id, then project id) whose
// fields are given. hidden's values are blank to the formulas, and their
// own values are left out. One budget for the whole read.
func workOut(ctx context.Context, tasks []*dgraphStruct.DgraphTask, values model.Values, projects map[string]string, fields []*model.Field, hidden map[string]bool) {
	rd := &formulaRead{fields: map[string][]*model.Field{}, byID: map[string]*model.Field{}, programs: map[string]*formula.Program{},
		budget: formula.NewBudget(), now: time.Now(), loc: helpers.ZoneOf(ctx), names: map[string]string{}}
	for _, f := range fields {
		p := strings.ToLower(f.ProjectID.String())
		rd.fields[p] = append(rd.fields[p], f)
		rd.byID[f.ID.String()] = f
	}
	for p, fs := range rd.fields {
		if hasFormula(fs) {
			rd.programs[p] = formula.Compile(inputs(fs)).Within(rd.budget)
		}
	}
	rd.readPeople(ctx, tasks, values, projects)
	for _, t := range tasks {
		if t == nil {
			continue
		}
		program := rd.programs[strings.ToLower(projects[strings.ToLower(t.Uuid)])]
		if program == nil {
			continue
		}
		own := values[t.Uuid]
		row := formula.Row{ID: t.Uuid}
		if t.CreatedAt != nil {
			row.Created = *t.CreatedAt
		}
		if t.UpdatedAt != nil {
			row.Modified = *t.UpdatedAt
		}
		out := program.RunRow(row, func(id string) (formula.Value, int) {
			if b, ok := builtinByID[id]; ok {
				return b.read(t, rd.loc), 1
			}
			f := rd.byID[id]
			if f == nil || hidden[id] {
				return formula.Blank(), 0
			}
			raw := own[id]
			return fieldTypes.FormulaValue(f.Type, rd.plain(f, raw)), formula.ReadCost(string(raw))
		}, rd.now, rd.loc)
		for id, v := range out {
			if hidden[id] {
				continue
			}
			b, err := json.Marshal(v.JSON())
			if err != nil || string(b) == "null" {
				continue
			}
			if own == nil {
				own = map[string]json.RawMessage{}
				values[t.Uuid] = own
			}
			own[id] = b
		}
	}
}

// plain is a stored value as a formula reads it, in a table's cell's shape
// (fieldTypes.FormulaValue): an option by its label, several as a list,
// money in its units, a person by name. Nil for none.
func (rd *formulaRead) plain(f *model.Field, raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	switch f.Type {
	case TypeMoney:
		if cents, ok := v.(float64); ok {
			return cents / 100
		}
		return nil
	case TypeSelect:
		s, _ := v.(string)
		return optionLabel(f, s)
	case TypeMultiSelect:
		ids, _ := v.([]any)
		labels := make([]any, 0, len(ids))
		for _, id := range ids {
			if s, ok := id.(string); ok {
				if l := optionLabel(f, s); l != "" {
					labels = append(labels, l)
				}
			}
		}
		return labels
	case TypePerson:
		s, _ := v.(string)
		return rd.names[s]
	}
	return v
}

// readPeople names, in one read, the people in person fields a formula
// reads; only when one does.
func (rd *formulaRead) readPeople(ctx context.Context, tasks []*dgraphStruct.DgraphTask, values model.Values, projects map[string]string) {
	read := map[string]bool{}
	for p, program := range rd.programs {
		for _, f := range rd.fields[p] {
			if f.Type != TypeFormula {
				continue
			}
			refs, _ := program.Reads(f.ID.String())
			for _, ref := range refs {
				if g := rd.byID[ref]; g != nil && g.Type == TypePerson {
					read[ref] = true
				}
			}
		}
	}
	if len(read) == 0 {
		return
	}
	var ids []string
	seen := map[string]bool{}
	for _, t := range tasks {
		if t == nil || projects[strings.ToLower(t.Uuid)] == "" {
			continue
		}
		for id := range read {
			var s string
			if json.Unmarshal(values[t.Uuid][id], &s) == nil && s != "" && !seen[s] {
				seen[s] = true
				ids = append(ids, s)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	people, err := userDomain.GetUserDisplayMapByUUIDs(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/TaskField formula people err: %+v", err)
		return
	}
	for id, u := range people {
		if u != nil {
			rd.names[id] = u.DisplayName()
		}
	}
}

// resultTypes is the kind of field a formula's result reads as.
var resultTypes = map[formula.Kind]string{formula.KindNumber: TypeNumber, formula.KindDate: TypeDate, formula.KindBool: TypeCheckbox}

// ResultTypes is, for each formula among fields (of one project or many),
// the kind of field its values read as: number, date, checkbox, or text,
// for code that reads values by their kind (the report's slices). Pure.
func ResultTypes(fields []*model.Field) map[string]string {
	out := map[string]string{}
	byProject := map[uuid.UUID][]*model.Field{}
	for _, f := range fields {
		if f != nil {
			byProject[f.ProjectID] = append(byProject[f.ProjectID], f)
		}
	}
	for _, fs := range byProject {
		if !hasFormula(fs) {
			continue
		}
		program := formula.Compile(inputs(fs))
		for _, f := range fs {
			if f.Type != TypeFormula {
				continue
			}
			out[f.ID.String()] = TypeText
			if t, ok := resultTypes[program.Kind(f.ID.String())]; ok {
				out[f.ID.String()] = t
			}
		}
	}
	return out
}
