// Package business (TaskTemplate): templates to make one task from (linear-09).
//
// A template is a task's shape: what a project template keeps for each of its
// tasks (business/ProjectTemplate Task: a name, a body, a status, a priority,
// tags, days counted from the day it is made, and sub-tasks), with a type, an
// assignee, values of the project's task fields, and the properties its maker
// must fill (business/TaskWriter's property keys, the words forms use too). A
// template can be the default for a type, which a task of that type starts
// from.
//
// WHO. A project's templates are its admins' to keep, change, delete and
// restore (Work Access NeedManage, which names templates); the workspace's
// are the workspace admins'. Anyone who can make a task in a project
// (NeedCreateTask) makes one from either, and only they see the list: a
// guest, who reaches the workspace only through a grant, never does. A task
// made from a template is copied from it, never linked, so changing or
// deleting a template never changes a task, and deleting one is a soft
// delete its keepers can undo.
//
// Making one goes through the Task Writer (business/TaskWriter MakeTask), the
// one way forms and rules make tasks too, so a template's task is checked,
// logged, searched and told about as one made in the app. A preset the
// project can't take (an assignee who isn't in it, a field or a type it
// doesn't have) is left off and named, never dropped without a word.
package business

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	projectTemplate "github.com/akashc777/OneCamp/business/ProjectTemplate"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// Limits on a template.
const (
	MaxNameLength  = 60
	MaxAboutLength = 280
	MaxSubtasks    = 50
	MaxFields      = 50
	MaxRequired    = 20
	// MaxKept is the most live templates a project, or the workspace, keeps.
	MaxKept = 100
)

// Shape is what a template fills in on the task it makes.
type Shape struct {
	projectTemplate.Task
	// Type is a type's id: a built-in one or the project's own.
	Type string `json:"type,omitempty"`
	// Assignee is a person's or an AI teammate's id.
	Assignee string `json:"assignee,omitempty"`
	// Fields are values of the project's task fields by the field's id, each
	// as the field keeps it (TaskField Normalize).
	Fields map[string]json.RawMessage `json:"fields,omitempty"`
	// Required are the properties its maker must fill (taskWriter.Prop*).
	Required []string `json:"required,omitempty"`
}

// Template is a task template. ProjectID is "" for a workspace's.
type Template struct {
	ID          string `json:"id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// DefaultFor is the type it is the default for, which its tasks have.
	DefaultFor string `json:"default_for,omitempty"`
	Shape      Shape  `json:"shape"`
}

// TemplateError is a template that can't be kept as it is; its text says
// what to fix.
type TemplateError struct{ msg string }

func (e *TemplateError) Error() string { return e.msg }

func fix(format string, a ...any) error { return &TemplateError{fmt.Sprintf(format, a...)} }

// anID is s as an id kept in lower case, "" for none, or false when it isn't one.
func anID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return "", false
	}
	return id.String(), true
}

// Check is the template tidied as it is kept, or what to fix. Pure.
func Check(t Template) (Template, error) {
	t.Name = strings.TrimSpace(t.Name)
	switch n := utf8.RuneCountInString(t.Name); {
	case n == 0:
		return t, fix("A template needs a name.")
	case n > MaxNameLength:
		return t, fix("A template's name takes at most %d characters.", MaxNameLength)
	}
	t.Description = strings.TrimSpace(t.Description)
	if utf8.RuneCountInString(t.Description) > MaxAboutLength {
		return t, fix("A template's description takes at most %d characters.", MaxAboutLength)
	}
	var ok bool
	if t.ProjectID, ok = anID(t.ProjectID); !ok {
		return t, fix("That project isn't one OneCamp knows.")
	}
	if err := checkShape(&t.Shape); err != nil {
		return t, err
	}
	if t.DefaultFor, ok = anID(t.DefaultFor); !ok {
		return t, fix("A template is the default for a type, and that type isn't one OneCamp knows.")
	}
	if t.DefaultFor != "" {
		switch t.Shape.Type {
		case "":
			t.Shape.Type = t.DefaultFor
		case t.DefaultFor:
		default:
			return t, fix("A template that is the default for a type makes tasks of that type: set its type to match.")
		}
	}
	return t, nil
}

func checkShape(s *Shape) error {
	// The task's name is where a made task's name starts (the maker types
	// after "Bug: "), so it is kept as typed; one of only spaces is none.
	// The Task Writer tidies the name a task is made with (cleanName).
	if strings.TrimSpace(s.Name) == "" {
		s.Name = ""
	}
	if utf8.RuneCountInString(s.Name) > projectTemplate.MaxTaskName {
		return fix("A task's name takes at most %d characters.", projectTemplate.MaxTaskName)
	}
	if len(s.Description) > projectTemplate.MaxTaskDescription {
		return fix("The task's description is too long: keep it under %d characters.", projectTemplate.MaxTaskDescription)
	}
	s.Status, s.Tags = strings.TrimSpace(s.Status), strings.TrimSpace(s.Tags)
	s.Priority = strings.ToLower(strings.TrimSpace(s.Priority))
	if !taskPriority.IsOrNone(s.Priority) {
		return fix("%q isn't one of the priorities.", s.Priority)
	}
	for _, d := range []*int{s.StartDay, s.DueDay} {
		if d != nil && (*d < 0 || *d > projectTemplate.MaxDay) {
			return fix("A day counts from the day the task is made, from 0 to %d.", projectTemplate.MaxDay)
		}
	}
	if s.StartDay != nil && s.DueDay != nil && *s.DueDay < *s.StartDay {
		return fix("The task is due before it starts: make its due day %d or later.", *s.StartDay)
	}
	if len(s.Subtasks) > MaxSubtasks {
		return fix("A template makes at most %d sub-tasks.", MaxSubtasks)
	}
	for i := range s.Subtasks {
		st := &s.Subtasks[i]
		st.Name = strings.TrimSpace(st.Name)
		switch {
		case st.Name == "":
			return fix("The sub-task at %d needs a name.", i+1)
		case utf8.RuneCountInString(st.Name) > projectTemplate.MaxTaskName:
			return fix("The sub-task at %d takes a name of at most %d characters.", i+1, projectTemplate.MaxTaskName)
		case st.DueDay != nil:
			return fix("The sub-task %q has a due day, which a task template doesn't keep: take it off.", st.Name)
		}
	}
	var ok bool
	if s.Type, ok = anID(s.Type); !ok {
		return fix("The template's type isn't one OneCamp knows.")
	}
	if s.Assignee, ok = anID(s.Assignee); !ok {
		return fix("The template's assignee isn't someone OneCamp knows.")
	}
	if len(s.Fields) > MaxFields {
		return fix("A template sets at most %d fields.", MaxFields)
	}
	if len(s.Fields) > 0 {
		fields := make(map[string]json.RawMessage, len(s.Fields))
		for k, v := range s.Fields {
			id, ok := anID(k)
			if !ok || id == "" {
				return fix("%q isn't one of the project's fields.", k)
			}
			if !json.Valid(v) {
				return fix("The value for the field %s isn't one a field keeps.", id)
			}
			if !taskWriter.IsNull(v) {
				fields[id] = v
			}
		}
		s.Fields = fields
	}
	if len(s.Required) > MaxRequired {
		return fix("A template asks for at most %d properties.", MaxRequired)
	}
	var required []string
	for _, k := range s.Required {
		p := taskWriter.PropOf(k)
		if p == "" {
			return fix("%q isn't a property a task can be asked for.", strings.TrimSpace(k))
		}
		if !slices.Contains(required, p) {
			required = append(required, p)
		}
	}
	s.Required = required
	return nil
}

// Facts is what a project takes, read once before a task is made in it.
type Facts struct {
	// Fields are the project's task fields.
	Fields []*taskFieldModel.Field
	// Types are the project's own types; the built-in ones always fit.
	Types []uuid.UUID
	// Statuses are the project's statuses, by key and by name.
	Statuses []string
	// Assignable are the people and AI teammates who may be given its tasks,
	// of those the template and the maker name.
	Assignable map[string]bool
	// Money is whether the maker may set money values, as an admin of the
	// project (business/TaskField MaySet): what anyone else types in a money
	// field is left off, and the template's own value stands.
	Money bool
}

// Typed is what the maker typed over a template: nil keeps the template's
// value, and an empty one clears it. A field typed null is cleared. It is
// the body of a make request as the web sends it.
type Typed struct {
	Name        *string                    `json:"name,omitempty"`
	Description *string                    `json:"description,omitempty"`
	Priority    *string                    `json:"priority,omitempty"`
	Tags        *string                    `json:"tags,omitempty"`
	Status      *string                    `json:"status,omitempty"`
	Assignee    *string                    `json:"assignee,omitempty"`
	Type        *string                    `json:"type,omitempty"`
	Start       *time.Time                 `json:"start,omitempty"`
	Due         *time.Time                 `json:"due,omitempty"`
	ClearStart  bool                       `json:"clear_start,omitempty"`
	ClearDue    bool                       `json:"clear_due,omitempty"`
	Fields      map[string]json.RawMessage `json:"fields,omitempty"`
}

// Made is the task a template and what was typed over it make.
type Made struct {
	Task     taskWriter.NewTask
	Subtasks []string
	// LeftOff says, in words, each preset the project couldn't take.
	LeftOff []string
}

// Fit is the task a template makes in a project, as typed over, its days
// counted from start. Pure.
func Fit(t Template, typed Typed, facts Facts, start projectTemplate.Start) Made {
	s := t.Shape
	var m Made
	in := &m.Task
	left := func(format string, a ...any) { m.LeftOff = append(m.LeftOff, fmt.Sprintf(format, a...)) }
	// The template keeps its task's name as typed ("Bug: "); a task made
	// with nothing typed after it is named without the trailing space.
	in.Name = strings.TrimSpace(pick(typed.Name, s.Name))
	in.Description = pick(typed.Description, s.Description)
	in.Priority = pick(typed.Priority, s.Priority)
	in.Label = pick(typed.Tags, s.Tags)
	if st := pick(typed.Status, s.Status); st != "" {
		if slices.ContainsFunc(facts.Statuses, func(have string) bool { return strings.EqualFold(have, st) }) {
			in.Status = st
		} else {
			left("The status %q: this project has no such status, so the task starts in its first one.", st)
		}
	}
	in.Start = when(typed.Start, typed.ClearStart, s.StartDay, start, projectTemplate.StartHour)
	in.Due = when(typed.Due, typed.ClearDue, s.DueDay, start, projectTemplate.DueHour)
	if ty := pick(typed.Type, s.Type); ty != "" {
		id, err := uuid.Parse(ty)
		switch {
		case err != nil:
			left("The type: it isn't one OneCamp knows, so the task is a Task.")
		case id == workTaxonomy.TypeTask || workTaxonomy.Builtin(id) || slices.Contains(facts.Types, id):
			// Task is kept like any type chosen, so it meets a template's
			// required type; the Task Writer sets no type for it.
			in.Type = &id
		default:
			left("The type: this project doesn't have it, so the task is a Task.")
		}
	}
	if a := pick(typed.Assignee, s.Assignee); a != "" {
		if facts.Assignable[a] {
			in.Assignee = &dgraphStruct.DgraphUser{Uuid: a}
		} else {
			left("The assignee: they can't be given this project's tasks, so the task is unassigned.")
		}
	}
	fitFields(&m, s.Fields, typed.Fields, facts)
	for _, st := range s.Subtasks {
		m.Subtasks = append(m.Subtasks, st.Name)
	}
	return m
}

// pick is what was typed, or else the template's.
func pick(typed *string, preset string) string {
	if typed != nil {
		return strings.TrimSpace(*typed)
	}
	return preset
}

// when is a typed time, none when it was cleared, or else the template's day
// counted from start at hour.
func when(typed *time.Time, cleared bool, day *int, start projectTemplate.Start, hour int) *time.Time {
	switch {
	case cleared:
		return nil
	case typed != nil:
		return typed
	case day == nil:
		return nil
	}
	at, err := time.Parse(time.RFC3339, start.At(*day, hour))
	if err != nil {
		return nil
	}
	return &at
}

// fitFields sets each field value the project's field takes, typed over the
// template's (null clears one), and names each it can't take. A money value
// typed by someone who may not set money is left off (facts.Money).
func fitFields(m *Made, preset, typed map[string]json.RawMessage, facts Facts) {
	fields := facts.Fields
	values := map[string]json.RawMessage{}
	for k, v := range preset {
		values[k] = v
	}
	var refused []string
	for k, v := range typed {
		id, ok := anID(k)
		if !ok || id == "" {
			continue
		}
		if i := slices.IndexFunc(fields, func(f *taskFieldModel.Field) bool { return f.ID.String() == id }); i >= 0 && fields[i].Type == taskFieldModel.TypeMoney && !facts.Money {
			refused = append(refused, fields[i].Name)
			continue
		}
		values[id] = v
	}
	sort.Strings(refused)
	for _, name := range refused {
		m.LeftOff = append(m.LeftOff, fmt.Sprintf("The field %s: %s, so what was typed in it is left off.", name, taskFieldBusiness.ErrMoneyAdminsOnly))
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := values[k]
		if taskWriter.IsNull(v) {
			continue
		}
		i := slices.IndexFunc(fields, func(f *taskFieldModel.Field) bool { return f.ID.String() == k })
		if i < 0 {
			m.LeftOff = append(m.LeftOff, "The value of a field this project doesn't have is left off.")
			continue
		}
		f := fields[i]
		norm, err := taskFieldBusiness.Normalize(f, v)
		if err != nil {
			m.LeftOff = append(m.LeftOff, fmt.Sprintf("The field %s: %s, so it is left empty.", f.Name, strings.TrimSuffix(err.Error(), ".")))
			continue
		}
		if m.Task.Fields == nil {
			m.Task.Fields = map[uuid.UUID]json.RawMessage{}
		}
		m.Task.Fields[f.ID] = norm
	}
}

// MayManage says whether someone may keep, change, delete or restore t: a
// project's template takes NeedManage in it (s), and the project not
// archived; a workspace's takes a workspace admin. Pure.
func MayManage(t Template, s workAccess.Standing, isAdmin bool) bool {
	if t.ProjectID == "" {
		return isAdmin
	}
	return !s.Archived && s.Can(workAccess.NeedManage)
}

// MayApply says whether someone standing s in a project may list its
// templates and the workspace's, and make a task from one: whoever may make
// a task there, and never in an archived project. Pure.
func MayApply(s workAccess.Standing) bool {
	return s.AddsTasks()
}

// Defaults is, for each type a listed template is the default for, the id
// of the one a new task of that type starts from: the project's default for
// it, else the workspace's. Pure.
func Defaults(list []Summary) map[string]string {
	out := map[string]string{}
	for _, s := range list {
		if s.DefaultFor == "" {
			continue
		}
		if _, has := out[s.DefaultFor]; !has || s.ProjectID != "" {
			out[s.DefaultFor] = s.ID
		}
	}
	return out
}
