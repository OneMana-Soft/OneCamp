package business

// Answers that fill the task's own fields: a choice sets its priority, a date
// its due date, choices its tags, and any question one of the project's task
// fields of its kind. Every answer still goes into the description, so
// nothing a visitor wrote is lost when a field can't take it.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// What an answer can fill besides the description (Field.MapsTo).
const (
	MapPriority = "priority"
	MapDue      = "due"
	MapTags     = "tags"
	// MapField + a task field's id fills that field of the project.
	MapField = "field:"
)

// fills is which questions fill a field of each kind: a project's task field
// or a table's column, whose kinds share their names. A kind it doesn't name
// (a person, a link, a worked-out column) takes no answer.
var fills = map[string][]string{
	taskFieldModel.TypeText:        {ShortText, LongText, Email, Phone, URL},
	taskFieldModel.TypeURL:         {URL},
	tableModel.FieldEmail:          {Email},
	taskFieldModel.TypeNumber:      {Number},
	taskFieldModel.TypeMoney:       {Number},
	taskFieldModel.TypeDate:        {Date},
	taskFieldModel.TypeSelect:      {Select},
	taskFieldModel.TypeMultiSelect: {Select, MultiSelect},
	taskFieldModel.TypeCheckbox:    {Checkbox},
}

// checkMapsTo says what's wrong with what question n fills, as far as the
// question alone tells, or "". A field it fills is checked against the
// project's fields by checkFields. Pure.
func checkMapsTo(fl *Field, n int) string {
	if fl.MapsTo != MapPriority {
		fl.Map = nil
	}
	switch {
	case fl.MapsTo == "" || fl.MapsTo == "description":
		fl.MapsTo = ""
	case fl.MapsTo == MapPriority:
		if fl.Type != Select {
			return fmt.Sprintf("Question %d sets the priority, so it needs to be a choice.", n)
		}
		m := make(map[string]string, len(fl.Options))
		for _, o := range fl.Options {
			p := fl.Map[o]
			if !taskPriority.Is(p) {
				return fmt.Sprintf("Question %d: say which priority “%s” sets.", n, o)
			}
			m[o] = p
		}
		fl.Map = m
	case fl.MapsTo == MapDue:
		if fl.Type != Date {
			return fmt.Sprintf("Question %d sets the due date, so it needs to be a date.", n)
		}
	case fl.MapsTo == MapTags:
		if fl.Type != Select && fl.Type != MultiSelect {
			return fmt.Sprintf("Question %d sets tags, so it needs to be a choice.", n)
		}
	case strings.HasPrefix(fl.MapsTo, MapField):
		id, err := uuid.Parse(strings.TrimPrefix(fl.MapsTo, MapField))
		if err != nil {
			return fmt.Sprintf("Question %d fills a field this project doesn't have.", n)
		}
		fl.MapsTo = MapField + id.String()
	default:
		return fmt.Sprintf("Question %d fills something a form can't fill.", n)
	}
	return ""
}

// fieldOf is the project field a question fills, or uuid.Nil.
func fieldOf(fl Field) uuid.UUID {
	if !strings.HasPrefix(fl.MapsTo, MapField) {
		return uuid.Nil
	}
	id, _ := uuid.Parse(strings.TrimPrefix(fl.MapsTo, MapField))
	return id
}

// fillsFields says whether any question fills one of the project's fields.
func fillsFields(fields []Field) bool {
	return slices.ContainsFunc(fields, func(f Field) bool { return fieldOf(f) != uuid.Nil })
}

// checkFields checks each question that fills a project field against the
// field: it must be the project's, of a kind the question can fill, and a
// choice takes the field's own options, so what a visitor picks is always
// one of them. The fields are read once. Pure but for that read.
func checkFields(ctx context.Context, project uuid.UUID, fields []Field) error {
	if !fillsFields(fields) {
		return nil
	}
	have, err := projectFields(ctx, project)
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID]*taskFieldModel.Field, len(have))
	for _, f := range have {
		byID[f.ID] = f
	}
	for i := range fields {
		id := fieldOf(fields[i])
		if id == uuid.Nil {
			continue
		}
		f := byID[id]
		if f == nil {
			return &FormError{fmt.Sprintf("“%s” fills a field this project doesn't have.", fields[i].Label)}
		}
		if !slices.Contains(fills[f.Type], fields[i].Type) {
			return &FormError{fmt.Sprintf("“%s” can't fill %s: ask it as %s.", fields[i].Label, f.Name, kindsFor(f.Type))}
		}
		if f.Type == taskFieldModel.TypeSelect || f.Type == taskFieldModel.TypeMultiSelect {
			opts := make([]string, 0, len(f.Options))
			for _, o := range f.Options {
				opts = append(opts, o.Label)
			}
			if len(opts) < 2 || len(opts) > maxChoices {
				return &FormError{fmt.Sprintf("%s needs 2 to %d options for a form to offer.", f.Name, maxChoices)}
			}
			fields[i].Options = opts
		}
	}
	return nil
}

// kindsFor says which questions fill a field kind, in words.
func kindsFor(typ string) string {
	switch typ {
	case taskFieldModel.TypeURL:
		return "a link"
	case tableModel.FieldEmail:
		return "an email address"
	case taskFieldModel.TypeNumber, taskFieldModel.TypeMoney:
		return "a number"
	case taskFieldModel.TypeDate:
		return "a date"
	case taskFieldModel.TypeSelect:
		return "a choice"
	case taskFieldModel.TypeMultiSelect:
		return "a choice or several"
	case taskFieldModel.TypeCheckbox:
		return "a checkbox"
	}
	return "an answer in words"
}

// projectFields is a project's task fields; a seam for tests.
var projectFields = func(ctx context.Context, project uuid.UUID) ([]*taskFieldModel.Field, error) {
	return taskFieldBusiness.List(ctx, project)
}

// Values is what a submission sets on its task besides its name and
// description.
type Values struct {
	Priority string
	Due      *time.Time
	Tags     []string
	Fields   map[uuid.UUID]json.RawMessage
}

// Fill reads what the answers set on the task: the priority a choice maps
// to, a due date as the midnight that day starts where the visitor is (loc),
// tags, and the project's fields by their kind, each value as the field
// keeps it (TaskField Normalize). A value a field can't take, such as a
// choice the field has since lost, is left out. Pure.
func Fill(fields []Field, answers []Answer, loc *time.Location, project []*taskFieldModel.Field) Values {
	var v Values
	byQ := make(map[string]Field, len(fields))
	for _, f := range fields {
		byQ[f.Id] = f
	}
	byID := make(map[uuid.UUID]*taskFieldModel.Field, len(project))
	for _, f := range project {
		byID[f.ID] = f
	}
	for _, a := range answers {
		q, ok := byQ[a.Id]
		if !ok || q.MapsTo == "" {
			continue
		}
		switch q.MapsTo {
		case MapPriority:
			if p := q.Map[a.Value]; p != "" && v.Priority == "" {
				v.Priority = p
			}
		case MapDue:
			if d, err := time.ParseInLocation(time.DateOnly, a.Value, loc); err == nil && v.Due == nil {
				v.Due = &d
			}
		case MapTags:
			if len(a.Values) > 0 {
				v.Tags = append(v.Tags, a.Values...)
			} else if a.Value != "" {
				v.Tags = append(v.Tags, a.Value)
			}
		default:
			f := byID[fieldOf(q)]
			if f == nil {
				continue
			}
			raw := fieldValue(f, a)
			if raw == nil {
				continue
			}
			value, err := taskFieldBusiness.Normalize(f, raw)
			if err != nil || value == nil {
				continue
			}
			if v.Fields == nil {
				v.Fields = map[uuid.UUID]json.RawMessage{}
			}
			v.Fields[f.ID] = value
		}
	}
	return v
}

// fieldValue is an answer as a field of f's type is sent one, or nil.
func fieldValue(f *taskFieldModel.Field, a Answer) json.RawMessage {
	optionID := func(label string) string {
		for _, o := range f.Options {
			if strings.EqualFold(o.Label, label) {
				return o.ID
			}
		}
		return ""
	}
	var v any
	switch f.Type {
	case taskFieldModel.TypeNumber:
		if a.Num == nil {
			return nil
		}
		v = *a.Num
	case taskFieldModel.TypeMoney:
		if a.Num == nil {
			return nil
		}
		v = math.Round(*a.Num * 100)
	case taskFieldModel.TypeSelect:
		id := optionID(a.Value)
		if id == "" {
			return nil
		}
		v = id
	case taskFieldModel.TypeMultiSelect:
		labels := a.Values
		if len(labels) == 0 && a.Value != "" {
			labels = []string{a.Value}
		}
		ids := []string{}
		for _, l := range labels {
			if id := optionID(l); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		v = ids
	case taskFieldModel.TypeCheckbox:
		v = a.Value == "Yes"
	default:
		v = a.Value
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
