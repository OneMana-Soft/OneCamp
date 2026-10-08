package business

// A source project's custom fields become its OneCamp project's own fields
// (business/TaskField), and each imported task's values follow them.
//
// A field is found again rather than made twice: by this import's id map (a
// retried or later chunk), then by the workspace map (an earlier import of
// the same workspace), then by name and type among the project's fields (one
// someone made by hand), skipping a field this import already gave another
// source field. Options are matched by name.
//
// A task's values are read before the task is written. One its field can't
// keep (a project at its limit of fields, a link that isn't one, a person who
// wasn't imported) goes into the task's description instead, so nothing the
// task held is lost; the import's Errors say which. Values are set without
// task history: an import isn't someone changing a task.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskField "github.com/akashc777/OneCamp/business/TaskField"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// fieldLocks has one lock per OneCamp project, held while a field is found or
// made, so two chunks of one project never both make it. (Options are added
// under the field's own row lock: taskField.AddOptions.) Every import worker
// runs in this process.
var fieldLocks sync.Map

func lockFields(project uuid.UUID) func() {
	m, _ := fieldLocks.LoadOrStore(project, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// warnedFor holds each running import's warnings already logged, so one shows
// once an import rather than once a chunk (the subtask stage runs a chunk for
// every parent task).
var warnedFor sync.Map // job id → *jobWarnings

type jobWarnings struct {
	keys sync.Map
	last atomic.Int64 // unix seconds of its latest warning
}

// forgetFieldWarnings lets go of an import's warnings once it ends. An import
// that ends some other way is let go a day after its last warning (warn).
func forgetFieldWarnings(job uuid.UUID) { warnedFor.Delete(job) }

// staleWarnings is how long an import's warnings are kept after its last one.
const staleWarnings = 24 * time.Hour

// projectFields is one project's fields as one chunk sees them.
type projectFields struct {
	job        *importModels.Job
	chunk      *uuid.UUID
	projectSrc string
	project    uuid.UUID
	by         uuid.UUID // the importing user: who made the fields and set the values
	// fields maps a source field's id to its OneCamp field, nil for one that
	// can't come across.
	fields map[string]*taskFieldModel.Field
}

func newProjectFields(job *importModels.Job, chunk *uuid.UUID, projectSrc string, project, by uuid.UUID) *projectFields {
	return &projectFields{job: job, chunk: chunk, projectSrc: projectSrc, project: project, by: by,
		fields: map[string]*taskFieldModel.Field{}}
}

// fieldKey is a field's source id in the id maps. OneCamp's fields belong to
// a project, but a source may share one field across projects (Asana, Jira)
// or reuse an id on every board (monday.com's "status").
func fieldKey(projectSrc, fieldSrc string) string { return projectSrc + "\x1f" + fieldSrc }

// ensure makes a project's fields, in the source's order, when the project is
// imported, so it has them all, whether any task uses them or not.
func (pf *projectFields) ensure(ctx context.Context, defs []importProvider.SourceField) {
	for _, d := range defs {
		pf.field(ctx, d)
	}
}

// field is the OneCamp field for a source field, found or made; nil when it
// can't come across (a nameless field, a deleted one, or a project with as
// many as it can have). A failure that may pass (the database busy) isn't
// remembered: the next value tries again.
func (pf *projectFields) field(ctx context.Context, d importProvider.SourceField) *taskFieldModel.Field {
	if f, ok := pf.fields[d.SourceID]; ok {
		return f
	}
	if !slices.Contains(taskField.Types, d.Type) {
		pf.fields[d.SourceID] = nil // no field holds it: its values go to the description
		return nil
	}
	unlock := lockFields(pf.project)
	f, err := pf.findOrMake(ctx, d)
	unlock()
	var in *taskField.InputError
	if err != nil {
		pf.warn(ctx, "FIELD_SKIPPED", d.SourceID, "", fmt.Sprintf("The field %q wasn't brought across: %s%s", d.Name, reason(err), asText))
		if !errors.As(err, &in) {
			return nil
		}
	}
	pf.fields[d.SourceID] = f
	return f
}

// reason is an error as the person running the import reads it.
func reason(err error) string {
	var in *taskField.InputError
	if errors.As(err, &in) {
		return in.Error()
	}
	return "it couldn't be saved."
}

func (pf *projectFields) findOrMake(ctx context.Context, d importProvider.SourceField) (*taskFieldModel.Field, error) {
	key := fieldKey(pf.projectSrc, d.SourceID)
	if id, err := importModels.LookupIdMapping(ctx, pf.job.Id, importModels.EntityField, key); err != nil {
		return nil, err
	} else if id != uuid.Nil {
		return pf.mapped(ctx, id)
	}
	name := taskField.FitName(d.Name)
	if name == "" || !slices.Contains(taskField.Types, d.Type) {
		return nil, nil
	}
	// An earlier import of this workspace made or found it.
	if id, _ := importModels.LookupWorkspaceMapping(ctx, pf.job.Provider, pf.job.SourceWorkspaceName, importModels.EntityField, key); id != uuid.Nil {
		f, err := pf.mapped(ctx, id)
		if f != nil {
			pf.record(ctx, key, d, f, false)
		} else if err == nil {
			pf.warn(ctx, "FIELD_SKIPPED", d.SourceID, "deleted", fmt.Sprintf(
				"The field %q was deleted in OneCamp after an earlier import, so it wasn't made again.%s", d.Name, asText))
		}
		return f, err
	}
	have, err := taskField.List(ctx, pf.project)
	if err != nil {
		return nil, err
	}
	for _, f := range have {
		if !sameField(f, name, d) {
			continue
		}
		taken, err := pf.givenToAnother(ctx, f.ID, key)
		if err != nil {
			return nil, err
		}
		if !taken {
			pf.record(ctx, key, d, f, false)
			return f, nil
		}
	}
	options, dropped := optionInputs(d.Options)
	f, err := taskField.Create(ctx, pf.project, taskField.Input{Name: freeName(have, name), Type: d.Type, Currency: d.Currency, Options: options}, pf.by, "")
	if err != nil {
		return nil, err
	}
	// Recorded as this import's, so a rollback can take it away again.
	pf.record(ctx, key, d, f, true)
	if dropped > 0 {
		pf.warn(ctx, "FIELD_OPTIONS_CAPPED", d.SourceID, "", fmt.Sprintf(
			"%q has %d more options than a field can have (%d), so values naming them weren't kept as a field.%s", d.Name, dropped, taskField.MaxOptions, asText))
	}
	return f, nil
}

// mapped is the field an id map names, or nil (and no error) when someone has
// deleted it since: it stays deleted.
func (pf *projectFields) mapped(ctx context.Context, id uuid.UUID) (*taskFieldModel.Field, error) {
	f, err := taskFieldModel.Get(ctx, pf.project, id)
	if errors.Is(err, taskFieldModel.ErrNotFound) {
		return nil, nil
	}
	return f, err
}

// record maps a source field to its OneCamp field for this import and for
// later imports of the same workspace.
func (pf *projectFields) record(ctx context.Context, key string, d importProvider.SourceField, f *taskFieldModel.Field, made bool) {
	_ = importModels.UpsertIdMappingWithOwnership(ctx, pf.job.Id, importModels.EntityField, key, f.ID, nil,
		mustMarshal(map[string]any{"name": d.Name, "project_source_id": pf.projectSrc}), made)
	_ = importModels.UpsertWorkspaceMapping(ctx, pf.job.Provider, pf.job.SourceWorkspaceName, importModels.EntityField, key, f.ID, pf.job.Id)
}

// givenToAnother reports whether this import already mapped a field to a
// different source field: two of a source's fields with one name (common on
// Jira sites) stay two fields.
func (pf *projectFields) givenToAnother(ctx context.Context, field uuid.UUID, key string) (bool, error) {
	keys, err := importModels.SourceIdsFor(ctx, pf.job.Id, importModels.EntityField, field)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if k != key {
			return true, nil
		}
	}
	return false, nil
}

// sameField reports whether a project's field is the source field: the same
// name and type (and, for money, currency). Pure.
func sameField(f *taskFieldModel.Field, name string, d importProvider.SourceField) bool {
	return strings.EqualFold(f.Name, name) && f.Type == d.Type &&
		(d.Type != importProvider.FieldMoney || d.Currency == "" || f.Currency == strings.ToUpper(d.Currency))
}

// freeName is name, or name with a number after it when the project has a
// field of that name already. Pure.
func freeName(have []*taskFieldModel.Field, name string) string {
	taken := map[string]bool{}
	for _, f := range have {
		taken[strings.ToLower(f.Name)] = true
	}
	if !taken[strings.ToLower(name)] {
		return name
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf(" (%d)", n)
		try := taskField.FitName(string([]rune(name)[:min(len([]rune(name)), taskField.MaxNameLength-len(suffix))]) + suffix)
		if !taken[strings.ToLower(try)] {
			return try
		}
	}
}

// optionInputs is a source field's options as a new field takes them: names
// fitted, without blanks or namesakes, as many as a field can have; dropped
// is how many distinct options didn't fit. Pure.
func optionInputs(opts []importProvider.SourceOption) (out []taskField.OptionInput, dropped int) {
	seen := map[string]bool{}
	for _, o := range opts {
		label := taskField.FitLabel(o.Label)
		if label == "" || seen[strings.ToLower(label)] {
			continue
		}
		seen[strings.ToLower(label)] = true
		if len(out) == taskField.MaxOptions {
			dropped++
			continue
		}
		out = append(out, taskField.OptionInput{Label: label, Color: o.Color})
	}
	return out, dropped
}

// readyValue is a value its field will keep, as stored.
type readyValue struct {
	src   string // the source field's id
	field *taskFieldModel.Field
	raw   json.RawMessage
}

// lostValue is a value no field can keep, as the task's description shows it.
type lostValue struct{ name, text string }

// prepare reads an imported task's values before the task is written: the
// ones its fields will keep, and the ones they can't, for its description.
func (pf *projectFields) prepare(ctx context.Context, values []importProvider.SourceFieldValue) (ready []readyValue, lost []lostValue) {
	for _, v := range values {
		f := pf.field(ctx, v.Field)
		if f == nil {
			lost = appendLost(lost, v)
			continue
		}
		raw, problem := pf.encode(ctx, f, v)
		if problem == "" && raw != nil {
			stored, err := taskField.Normalize(f, raw)
			switch {
			case err != nil:
				problem = reason(err)
			case stored != nil:
				ready = append(ready, readyValue{v.Field.SourceID, f, stored})
			}
		}
		if problem != "" {
			pf.warn(ctx, "FIELD_VALUE_SKIPPED", v.Field.SourceID, problem, fmt.Sprintf(
				"Some values of %q weren't kept as a field: %s%s", f.Name, problem, asText))
			lost = appendLost(lost, v)
		}
	}
	return ready, lost
}

// write gives a task the values prepare readied.
func (pf *projectFields) write(ctx context.Context, task uuid.UUID, ready []readyValue) {
	for _, r := range ready {
		if err := taskFieldModel.SetValue(ctx, task, r.field.ID, r.raw, pf.by); err != nil {
			pf.warn(ctx, "FIELD_VALUE_SKIPPED", r.src, "save", fmt.Sprintf("Some values of %q weren't brought across: %s", r.field.Name, reason(err)))
		}
	}
}

// asText ends a warning about values that weren't kept as a field: the ones
// that could be shown as text are in the description (appendLost); a value
// with nothing to show for it (a person the source names only by an id) is
// not.
const asText = " Where the import could show them as text, they're in each task's description instead."

// appendLost adds a value to the ones a description lists, if there's
// anything to show for it and the source doesn't list it there already.
func appendLost(lost []lostValue, v importProvider.SourceFieldValue) []lostValue {
	name, text := strings.TrimSpace(v.Field.Name), valueText(v)
	if name == "" || text == "" || v.InDescription {
		return lost
	}
	return append(lost, lostValue{name, text})
}

// valueText is a value as a person reads it: the source's own text, or one
// made from the value.
func valueText(v importProvider.SourceFieldValue) string {
	if t := strings.TrimSpace(v.Text); t != "" {
		return t
	}
	label := func(id string) string {
		for _, o := range v.Field.Options {
			if o.SourceID == id {
				return o.Label
			}
		}
		return ""
	}
	switch x := v.Value.(type) {
	case string:
		switch v.Field.Type {
		case importProvider.FieldSelect:
			return label(x)
		case importProvider.FieldPerson:
			return "" // an id means nothing to a reader
		}
		return x
	case []string:
		var labels []string
		for _, id := range x {
			if l := label(id); l != "" {
				labels = append(labels, l)
			}
		}
		return strings.Join(labels, ", ")
	case float64:
		n := strconv.FormatFloat(x, 'f', -1, 64)
		if v.Field.Type == importProvider.FieldMoney && v.Field.Currency != "" {
			return n + " " + v.Field.Currency
		}
		return n
	case bool:
		if x {
			return "Yes"
		}
	}
	return ""
}

// renderLostValues lists, below a task's description, the values no field
// could keep.
func renderLostValues(lost []lostValue) string {
	if len(lost) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<p><strong>Imported fields</strong></p><ul>")
	for _, l := range lost {
		b.WriteString("<li><strong>")
		b.WriteString(html.EscapeString(l.name))
		b.WriteString(":</strong> ")
		b.WriteString(html.EscapeString(helpers.TruncateRunes(l.text, 2000)))
		b.WriteString("</li>")
	}
	b.WriteString("</ul>")
	return b.String()
}

// kindPhrase is a field type as "OneCamp couldn't read them as …" ends.
func kindPhrase(t string) string {
	switch t {
	case importProvider.FieldText:
		return "text"
	case importProvider.FieldMoney:
		return "an amount"
	case importProvider.FieldSelect, importProvider.FieldMultiSelect:
		return "options of the field"
	case importProvider.FieldCheckbox:
		return "a yes or no"
	case importProvider.FieldURL:
		return "a link"
	}
	return "a " + t
}

// encode is a source value as the field keeps it, before Normalize. raw is nil
// when there's nothing to keep (an unticked box); problem says why a value
// can't be kept.
func (pf *projectFields) encode(ctx context.Context, f *taskFieldModel.Field, v importProvider.SourceFieldValue) (raw json.RawMessage, problem string) {
	misread := fmt.Sprintf("OneCamp couldn't read them as %s.", kindPhrase(f.Type))
	switch f.Type {
	case importProvider.FieldSelect, importProvider.FieldMultiSelect:
		var src []string
		switch x := v.Value.(type) {
		case string:
			src = []string{x}
		case []string:
			src = x
		default:
			return nil, misread
		}
		ids, problem := pf.options(ctx, f, v.Field, src)
		if problem != "" || len(ids) == 0 {
			return nil, problem
		}
		if f.Type == importProvider.FieldSelect {
			return mustMarshal(ids[0]), ""
		}
		return mustMarshal(ids), ""
	case importProvider.FieldPerson:
		src, ok := v.Value.(string)
		if !ok {
			return nil, misread
		}
		user, _ := importModels.LookupIdMapping(ctx, pf.job.Id, importModels.EntityUser, src)
		if user == uuid.Nil {
			return nil, "they name people who weren't imported."
		}
		return mustMarshal(user.String()), ""
	case importProvider.FieldMoney, importProvider.FieldNumber:
		n, ok := v.Value.(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, misread
		}
		if f.Type == importProvider.FieldMoney {
			return mustMarshal(int64(math.Round(n * 100))), ""
		}
		return mustMarshal(n), ""
	case importProvider.FieldCheckbox:
		b, ok := v.Value.(bool)
		if !ok {
			return nil, misread
		}
		return mustMarshal(b), ""
	case importProvider.FieldURL:
		s, ok := v.Value.(string)
		if !ok {
			return nil, misread
		}
		return mustMarshal(linkOf(s)), ""
	case importProvider.FieldText:
		s, ok := v.Value.(string)
		if !ok {
			return nil, misread
		}
		if r := []rune(strings.TrimSpace(s)); len(r) > taskField.MaxTextLength {
			s = string(r[:taskField.MaxTextLength])
		}
		return mustMarshal(s), ""
	case importProvider.FieldDate:
		s, ok := v.Value.(string)
		if !ok {
			return nil, misread
		}
		return mustMarshal(s), ""
	}
	return nil, misread
}

// linkOf is a link as a field keeps it: "example.com/brief" gains https://,
// while anything that doesn't look like an address ("TBD", "mailto:…")
// stays as it is, for Normalize to turn away. Pure.
func linkOf(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); s == "" || strings.ContainsAny(s, " \t\n") || (err == nil && u.Scheme != "") {
		return s
	}
	u, err := url.Parse("https://" + s)
	if err != nil || u.User != nil || !strings.Contains(u.Hostname(), ".") || strings.HasSuffix(u.Hostname(), ".") {
		return s
	}
	return "https://" + s
}

// options is the field's option ids for source options, by name, adding any
// the field doesn't have yet while it has room; problem says why a value's
// options can't be kept.
func (pf *projectFields) options(ctx context.Context, f *taskFieldModel.Field, d importProvider.SourceField, src []string) (ids []string, problem string) {
	labels := map[string]importProvider.SourceOption{}
	for _, o := range d.Options {
		labels[o.SourceID] = o
	}
	var missing []taskField.OptionInput
	for _, s := range src {
		o, ok := labels[s]
		label := taskField.FitLabel(o.Label)
		if !ok || label == "" {
			return nil, "they name an option the source no longer lists."
		}
		if id := optionID(f, label); id != "" {
			ids = append(ids, id)
		} else {
			missing = append(missing, taskField.OptionInput{Label: label, Color: o.Color})
		}
	}
	if len(missing) == 0 {
		return ids, ""
	}
	// AddOptions locks the field's row itself, so options two chunks add both stay.
	grown, err := taskField.AddOptions(ctx, pf.project, f.ID, missing)
	if err != nil {
		return nil, reason(err)
	}
	*f = *grown
	for _, m := range missing {
		id := optionID(f, m.Label)
		if id == "" {
			return nil, fmt.Sprintf("the field already has as many options as it can (%d).", taskField.MaxOptions)
		}
		ids = append(ids, id)
	}
	return ids, ""
}

// optionID is the id of the field's option of that name, whatever its case.
func optionID(f *taskFieldModel.Field, label string) string {
	for _, o := range f.Options {
		if strings.EqualFold(o.Label, label) {
			return o.ID
		}
	}
	return ""
}

// warn tells the person running the import, once an import for each project,
// source field, code and detail (the problem a value had).
func (pf *projectFields) warn(ctx context.Context, code, sourceField, detail, message string) {
	now := time.Now().Unix()
	entry, loaded := warnedFor.LoadOrStore(pf.job.Id, &jobWarnings{})
	seen := entry.(*jobWarnings)
	seen.last.Store(now)
	if !loaded {
		// A new import: let go of any an import that ended oddly left behind.
		warnedFor.Range(func(job, other any) bool {
			if now-other.(*jobWarnings).last.Load() > int64(staleWarnings/time.Second) {
				warnedFor.Delete(job)
			}
			return true
		})
	}
	if _, dup := seen.keys.LoadOrStore(strings.Join([]string{code, pf.projectSrc, sourceField, detail}, "\x1f"), true); dup {
		return
	}
	importModels.LogImportError(ctx, pf.job.Id, pf.chunk, importModels.EntityField, sourceField, importModels.SeverityWarning, code, message, nil)
}
