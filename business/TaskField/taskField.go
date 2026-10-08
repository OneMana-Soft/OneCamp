// Package business (TaskField) is custom fields on tasks: a project's admins
// give its tasks their own fields (text, number, money, date, select,
// multi-select, person, checkbox, link) and set each task's value from its
// panel. Values live in Postgres beside the task (migration 192) and are laid
// over the task wherever a project's tasks are listed, so the list, the board
// and the panel show them; a filter on a field asks Postgres which tasks match.
package business

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The field types, as the app names them.
const (
	TypeText        = "text"
	TypeNumber      = "number"
	TypeMoney       = "money"
	TypeDate        = "date"
	TypeSelect      = "select"
	TypeMultiSelect = "multi_select"
	TypePerson      = "person"
	TypeCheckbox    = "checkbox"
	TypeURL         = "url"
)

// Types lists every field type.
var Types = []string{TypeText, TypeNumber, TypeMoney, TypeDate, TypeSelect, TypeMultiSelect, TypePerson, TypeCheckbox, TypeURL}

const (
	MaxPerProject  = 30
	MaxNameLength  = 40
	MaxOptions     = 50
	MaxOptionLabel = 40
	MaxTextLength  = 2000
	// MaxNumber bounds numbers and money (in hundredths) alike.
	MaxNumber = 1e15
	// DefaultCurrency is a money field's when neither it nor the project's
	// billing names one.
	DefaultCurrency = "USD"
)

var (
	ErrNameTaken = model.ErrNameTaken
	ErrNotFound  = model.ErrNotFound
	// ErrTaskDeleted is a value set on a task in the bin.
	ErrTaskDeleted = errors.New("that task is deleted")
)

// InputError is a request the person can fix; its text is written for them.
type InputError struct{ msg string }

func (e *InputError) Error() string { return e.msg }

func invalid(format string, a ...any) error { return &InputError{fmt.Sprintf(format, a...)} }

func validType(t string) bool {
	for _, k := range Types {
		if k == t {
			return true
		}
	}
	return false
}

func validColor(c string) bool {
	for _, k := range taskStatusBusiness.Colors {
		if k == c {
			return true
		}
	}
	return false
}

// OptionInput is one choice as the app sends it: an id it already has, or
// none for a new one.
type OptionInput struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Color string `json:"color"`
}

// Input is a field as the app sends it, to make or to change.
type Input struct {
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	Options  []OptionInput `json:"options"`
	Currency string        `json:"currency"`
	OnCard   bool          `json:"on_card"`
}

func cleanName(s string) string { return strings.Join(strings.Fields(s), " ") }

// newOptionID is a short random id: short, so a filter on an option fits the
// query value limit beside its field's id.
func newOptionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isOptionID(s string) bool {
	if len(s) != 8 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// Check is the field as stored, or what to fix. typ is the field's type: the
// one asked for when making it, its own when changing it. Pure.
func Check(in Input, typ string) (*model.Field, error) {
	name := cleanName(in.Name)
	if name == "" || utf8.RuneCountInString(name) > MaxNameLength {
		return nil, invalid("Give the field a name of up to %d characters.", MaxNameLength)
	}
	if !validType(typ) {
		return nil, invalid("Choose what kind of field it is.")
	}
	f := &model.Field{Name: name, Type: typ, Options: []model.Option{}, OnCard: in.OnCard}
	if typ == TypeSelect || typ == TypeMultiSelect {
		if len(in.Options) > MaxOptions {
			return nil, invalid("A field can have up to %d options.", MaxOptions)
		}
		seen := map[string]bool{}
		ids := map[string]bool{}
		for i, o := range in.Options {
			label := cleanName(o.Label)
			if label == "" || utf8.RuneCountInString(label) > MaxOptionLabel {
				return nil, invalid("Give each option a name of up to %d characters.", MaxOptionLabel)
			}
			if seen[strings.ToLower(label)] {
				return nil, invalid("%q is there twice. Give each option its own name.", label)
			}
			seen[strings.ToLower(label)] = true
			id := o.ID
			if !isOptionID(id) || ids[id] {
				id = newOptionID()
			}
			ids[id] = true
			color := o.Color
			if !validColor(color) {
				color = taskStatusBusiness.Colors[i%len(taskStatusBusiness.Colors)]
			}
			f.Options = append(f.Options, model.Option{ID: id, Label: label, Color: color})
		}
	}
	if typ == TypeMoney {
		cur := strings.ToUpper(strings.TrimSpace(in.Currency))
		if len(cur) != 3 || strings.Trim(cur, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			return nil, invalid("Choose the currency by its three-letter code, like USD, EUR or INR.")
		}
		f.Currency = cur
	}
	return f, nil
}

// Normalize is a value as stored for the field, nil to take the value off, or
// what to fix. An empty text, an unticked box and an empty multi-select are no
// value. A person's id is checked to be a person by SetValue. Pure.
func Normalize(f *model.Field, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	str := func() (string, error) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", invalid("%s takes text.", f.Name)
		}
		return strings.TrimSpace(s), nil
	}
	num := func() (float64, error) {
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, invalid("%s takes a number.", f.Name)
		}
		if math.Abs(v) > MaxNumber {
			return 0, invalid("That number is too large for %s.", f.Name)
		}
		return v, nil
	}
	option := func(id string) bool {
		for _, o := range f.Options {
			if o.ID == id {
				return true
			}
		}
		return false
	}
	switch f.Type {
	case TypeText, TypeURL:
		s, err := str()
		if err != nil || s == "" {
			return nil, err
		}
		if utf8.RuneCountInString(s) > MaxTextLength {
			return nil, invalid("Keep %s under %d characters.", f.Name, MaxTextLength)
		}
		if f.Type == TypeURL {
			u, err := url.Parse(s)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, invalid("%s takes a link starting with https://.", f.Name)
			}
		}
		return json.Marshal(s)
	case TypeNumber:
		v, err := num()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	case TypeMoney:
		v, err := num()
		if err != nil {
			return nil, err
		}
		if v != math.Trunc(v) {
			return nil, invalid("%s is kept in whole cents.", f.Name)
		}
		return json.Marshal(int64(v))
	case TypeDate:
		s, err := str()
		if err != nil || s == "" {
			return nil, err
		}
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return nil, invalid("%s takes a date.", f.Name)
		}
		return json.Marshal(s)
	case TypeSelect, TypePerson:
		s, err := str()
		if err != nil || s == "" {
			return nil, err
		}
		if f.Type == TypeSelect && !option(s) {
			return nil, invalid("That isn't one of %s's options any more.", f.Name)
		}
		if f.Type == TypePerson {
			id, err := uuid.Parse(s)
			if err != nil {
				return nil, invalid("%s takes a person.", f.Name)
			}
			s = id.String()
		}
		return json.Marshal(s)
	case TypeMultiSelect:
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			return nil, invalid("%s takes a list of its options.", f.Name)
		}
		// An option taken away while the app still showed it is left out,
		// not refused: refusing would leave the task's value stuck.
		out := []string{}
		seen := map[string]bool{}
		for _, id := range ids {
			if option(id) && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return json.Marshal(out)
	case TypeCheckbox:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, invalid("%s is ticked or not.", f.Name)
		}
		if !b {
			return nil, nil
		}
		return json.Marshal(true)
	}
	return nil, invalid("Choose what kind of field it is.")
}

// InputOf is a field as the app sends one: a template keeps a project's
// fields this way, options and their ids included.
func InputOf(f *model.Field) Input {
	in := Input{Name: f.Name, Type: f.Type, Currency: f.Currency, OnCard: f.OnCard}
	for _, o := range f.Options {
		in.Options = append(in.Options, OptionInput(o))
	}
	return in
}

// List is a project's fields in their order.
func List(ctx context.Context, project uuid.UUID) ([]*model.Field, error) {
	return model.List(ctx, project)
}

// Create adds a field to a project. A money field with no currency takes the
// project's billing currency, or USD.
func Create(ctx context.Context, project uuid.UUID, in Input, by uuid.UUID, billingCurrency string) (*model.Field, error) {
	if in.Type == TypeMoney && strings.TrimSpace(in.Currency) == "" {
		in.Currency = billingCurrency
		if in.Currency == "" {
			in.Currency = DefaultCurrency
		}
	}
	f, err := Check(in, in.Type)
	if err != nil {
		return nil, err
	}
	n, err := model.Count(ctx, project)
	if err != nil {
		return nil, err
	}
	if n >= MaxPerProject {
		return nil, invalid("A project can have up to %d fields.", MaxPerProject)
	}
	f.ProjectID = project
	return model.Create(ctx, f, by)
}

// Update changes a field's name, options, currency or place on cards; its
// type stays. Options taken away come off the tasks that had them, and
// Changed lists those tasks.
func Update(ctx context.Context, project, id uuid.UUID, in Input) (field *model.Field, changed []string, err error) {
	old, err := model.Get(ctx, project, id)
	if err != nil {
		return nil, nil, err
	}
	if in.Type == TypeMoney || old.Type == TypeMoney {
		if strings.TrimSpace(in.Currency) == "" {
			in.Currency = old.Currency
		}
	}
	f, err := Check(in, old.Type)
	if err != nil {
		return nil, nil, err
	}
	f.ID, f.ProjectID = old.ID, project
	return model.Update(ctx, f)
}

// Delete removes a field and every task's value of it.
func Delete(ctx context.Context, project, id uuid.UUID) error {
	return model.Delete(ctx, project, id)
}

// Reorder puts a project's fields in the order given.
func Reorder(ctx context.Context, project uuid.UUID, ids []uuid.UUID) error {
	return model.Reorder(ctx, project, ids)
}

// PersonExists says whether id is someone OneCamp knows; it's how a person
// field's value is checked. A variable so tests can stand in for Postgres.
var PersonExists = func(_ context.Context, id uuid.UUID) (bool, error) {
	found, err := userModels.GetActiveUsersByUUIDsForNotification([]uuid.UUID{id})
	return found[id] != nil, err
}

// SetValue gives a task its value of a field, or takes it off (raw null), and
// notes the change in the task's history. The field must be of the task's
// project, which the caller has checked. It returns the value as stored.
func SetValue(ctx context.Context, task *dgraphStruct.DgraphTask, f *model.Field, raw json.RawMessage, user *dgraphStruct.DgraphUser, by uuid.UUID) (json.RawMessage, error) {
	if task.DeletedAt != nil && task.DeletedAt.Year() > 1970 {
		return nil, ErrTaskDeleted
	}
	taskID, err := uuid.Parse(task.Uuid)
	if err != nil {
		return nil, err
	}
	value, err := Normalize(f, raw)
	if err != nil {
		return nil, err
	}
	if f.Type == TypePerson && value != nil {
		var s string
		_ = json.Unmarshal(value, &s)
		id, _ := uuid.Parse(s)
		ok, err := PersonExists(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, invalid("%s takes someone in the workspace.", f.Name)
		}
	}
	before, err := model.ValueOf(ctx, taskID, f.ID)
	if err != nil {
		return nil, err
	}
	if sameValue(before, value) {
		return value, nil
	}
	if err := model.SetValue(ctx, taskID, f.ID, value, by); err != nil {
		return nil, err
	}
	now := time.Now()
	if err := taskDomain.UpdateTaskByTaskUUID(ctx, taskID, now); err != nil {
		helpers.LogErrorWithContext(ctx, "business/TaskField SetValue postgres err: %+v", err)
	}
	history := &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: task.Uuid, UpdatedAt: &now,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid:      uuid.NewString(),
			Type:      dgraphStruct.ACTIVITY_TYPE_FIELD,
			CreatedBy: &dgraphStruct.DgraphUser{Uid: user.Uid},
			LogTime:   &now,
			NextState: f.Name,
		}}}
	// The value is saved; a history line that didn't make it is logged, not
	// turned into a failure the person would retry. Written only to a task
	// that exists: an upsert would make one for an unknown id.
	if err := taskDomain.UpdateDgraphTaskClearing(ctx, history, nil); err != nil {
		helpers.LogErrorWithContext(ctx, "business/TaskField SetValue history err: %+v", err)
	}
	return value, nil
}

func sameValue(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

// MergeFieldValues lays each task's field values over it, for the app.
func MergeFieldValues(ctx context.Context, tasks []*dgraphStruct.DgraphTask) error {
	ids := make([]uuid.UUID, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if id, err := uuid.Parse(t.Uuid); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	values, err := model.ValuesFor(ctx, ids)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t != nil {
			t.Fields = values[t.Uuid]
		}
	}
	return nil
}

// CopyValues gives a project's new task the values of another, for the
// fields it has none of yet: a repeating task's next copy.
func CopyValues(ctx context.Context, from, to, by uuid.UUID) error {
	return model.CopyValues(ctx, from, to, by)
}

// FilterPrefix starts a list filter's id on a field: "field_" and the field's
// id without dashes, the shape a filter id may take.
const FilterPrefix = "field_"

// FilterID is the list filter id for a field.
func FilterID(field uuid.UUID) string {
	return FilterPrefix + strings.ReplaceAll(field.String(), "-", "")
}

// View is a field as the app gets it, with the filter id that narrows a list
// to its values, so the app never builds one itself.
type View struct {
	*model.Field
	FilterID string `json:"filter_id"`
}

// Views is fields as the app gets them.
func Views(fields ...*model.Field) []View {
	out := make([]View, 0, len(fields))
	for _, f := range fields {
		if f != nil {
			out = append(out, View{Field: f, FilterID: FilterID(f.ID)})
		}
	}
	return out
}

// FieldOfFilter reads the field a filter id names.
func FieldOfFilter(id string) (uuid.UUID, bool) {
	if !strings.HasPrefix(id, FilterPrefix) {
		return uuid.Nil, false
	}
	f, err := uuid.Parse(strings.TrimPrefix(id, FilterPrefix))
	return f, err == nil
}

const (
	// FilterAny and FilterNone, as a filter value, ask for tasks with some
	// value of the field and with none.
	FilterAny  = "any"
	FilterNone = "none"
	// matchNothing is a clause no task matches.
	matchNothing = `eq(task_uuid, "00000000-0000-0000-0000-000000000000")`
)

// FilterClause narrows a project's task list to the tasks whose value of the
// field is one of the values given (an option id, a person's id, "true" for a
// ticked box), and with "any" or "none" to the tasks with some value or none.
// Values are ORed. A field that isn't the project's (since deleted, say)
// narrows nothing.
func FilterClause(ctx context.Context, project uuid.UUID, filterID string, values []string) (string, error) {
	fieldID, ok := FieldOfFilter(filterID)
	if !ok {
		return matchNothing, nil
	}
	if _, err := model.Get(ctx, project, fieldID); err != nil {
		// A field since deleted (a saved view, an old link) narrows nothing.
		if errors.Is(err, model.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	var asked []string
	wantAny, wantNone := false, false
	for _, v := range values {
		switch v {
		case FilterAny:
			wantAny = true
		case FilterNone:
			wantNone = true
		default:
			asked = append(asked, v)
		}
	}
	var parts []string
	if len(asked) > 0 {
		ids, err := model.TasksMatching(ctx, fieldID, model.Match{Values: asked})
		if err != nil {
			return "", err
		}
		parts = append(parts, inClause(ids))
	}
	if wantAny || wantNone {
		ids, err := model.TasksMatching(ctx, fieldID, model.Match{Any: true})
		if err != nil {
			return "", err
		}
		if wantAny {
			parts = append(parts, inClause(ids))
		}
		if wantNone {
			if len(ids) == 0 {
				parts = append(parts, "has(task_uuid)")
			} else {
				parts = append(parts, "NOT "+inClause(ids))
			}
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", nil
}

// inClause matches the tasks listed, or none.
func inClause(ids []string) string {
	if len(ids) == 0 {
		return matchNothing
	}
	b, _ := json.Marshal(ids)
	return fmt.Sprintf("eq(task_uuid, %s)", b)
}
