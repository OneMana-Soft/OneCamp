// Package business (FieldTypes) is tables' one registry of field kinds
// (design/tables-vs-airtable/PROGRAM.md §4.2 #60): what a cell of each kind
// holds, how what any writer sends becomes that, how a cell of one kind
// becomes another, and how any cell reads (cells.go). Every writer goes through
// it: the web, the API, AI tools and agents, imports, AI generation, Turn into
// database, forms and automations, through business/DataTable's one funnel
// (storedCells). Projects' task fields register their kinds here too.
//
// Each kind is registered once, in registry.go, with its rules in kinds.go.
// What reads a cell for one person (the names of linked items, a rollup, a
// formula) is business/DataTable's: a stored cell is the same for everyone, and
// this package knows only stored cells.
package business

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// Kind is one kind of field.
type Kind struct {
	// ID is the field's type as stored (data_table_fields.type).
	ID string
	// Stored is whether its cells live in a row's values: a formula's, a
	// rollup's and a link to a table's rows are worked out on each read.
	Stored bool
	// HasOptions is whether its cells are labels of the field's options.
	HasOptions bool
	// FormulaKind is how its cells read in a formula (a rollup's is what it
	// gives, which its config says).
	FormulaKind formula.Kind
	// Coerce is what a writer sent, as a cell of the field stores it, or why
	// it can't be one. A cell left empty ("", nil, []) never reaches it: an
	// empty cell clears, whatever the kind.
	Coerce func(f *model.Field, v any, in Input) (any, error)
	// Convert is a stored cell of another kind becoming one of this kind, as
	// a field's type changes: its value, whether it's kept as it is, and how
	// many values were cleared (ConvertCell).
	Convert func(v any, signs formula.NumberSigns) (out any, kept bool, cleared int)
	// NoDefault is a kind whose cells a new row can't start with.
	NoDefault bool
	// Config is the kind's settings in a field's config as stored, or why
	// they can't be (a rating's most stars, an id's prefix, a duration's
	// format, a status's options in their groups): it is given the config as
	// sent, before its options are planned, and gives it back as kept. Nil
	// for a kind with no settings of its own.
	Config func(cfg map[string]any) (map[string]any, error)
	// FromRow is, for a kind worked out from the row itself and never stored
	// (its number, when it was made and changed, and by whom), the cell a
	// row shows: a number, a time (RFC 3339), or a person as a link
	// ([]ItemRef of one), which each read names.
	FromRow func(f *model.Field, r *model.Row) any
}

// ConfigFor is a field's config as its kind keeps it (Kind.Config), or the
// config as it is for a kind with no settings.
func (r *Registry) ConfigFor(kind string, cfg map[string]any) (map[string]any, error) {
	k, ok := r.Lookup(kind)
	if !ok || k.Config == nil {
		return cfg, nil
	}
	return k.Config(cfg)
}

// Input is how a write's cells came.
type Input struct {
	// Signs are how the writer writes numbers; the zero value is the plain
	// signs (a point, commas between groups), how programs write them.
	Signs formula.NumberSigns
	// New is a row being made: a field it doesn't name gets its default.
	New bool
	// Complete is a write that must give every required field: a row made
	// through the API, an AI tool or a form; never the grid's blank new row.
	Complete bool
	// Unchanged is a whole row's cells as stored: a cell the write gives as
	// it is stored is kept, unchecked. The web released before patches sends
	// a cell edit as its copy of the whole row, and a cell stored before
	// writes were checked mustn't make that edit fail.
	Unchanged map[string]any
	// Files says which of ids are files of the row's table (an attachment
	// whose place is the table); nil refuses every file, as a writer that
	// can't ask mustn't name another place's.
	Files func(ids []string) (map[string]bool, error)
}

func (in Input) signs() formula.NumberSigns {
	if in.Signs.Decimal == "" {
		return formula.PlainSigns
	}
	return in.Signs
}

// CellError is one cell a write can't store, and why.
type CellError struct {
	Field string `json:"field"`
	Msg   string `json:"msg"`
}

// CellErrors is every cell of a write it can't store. Its message is the
// first's, which says which field and why.
type CellErrors []CellError

func (e CellErrors) Error() string {
	if len(e) == 0 {
		return "invalid row values"
	}
	return e[0].Msg
}

// Registry holds the kinds.
type Registry struct {
	mu    sync.RWMutex
	kinds map[string]Kind
	order []string
}

// Kinds is the one registry. Each kind registers once, in registry.go.
var Kinds = &Registry{}

// Register adds a kind. One with no id, or an id taken, panics: registration
// happens in init, where that fails the build's tests.
func (r *Registry) Register(k Kind) {
	if k.ID == "" {
		panic("field types: a kind needs an id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.kinds == nil {
		r.kinds = map[string]Kind{}
	}
	if _, taken := r.kinds[k.ID]; taken {
		panic(fmt.Sprintf("field types: the kind %q is registered twice", k.ID))
	}
	r.kinds[k.ID] = k
	r.order = append(r.order, k.ID)
}

// Lookup is a kind by its id.
func (r *Registry) Lookup(id string) (Kind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.kinds[id]
	return k, ok
}

// Valid is whether a field may be of kind id: a kind someone built and
// registered. The database takes more (migration 300 makes room for the kinds
// the programme builds), and nothing can read a field of a kind not here.
func (r *Registry) Valid(id string) bool {
	_, ok := r.Lookup(id)
	return ok
}

// All is every kind, in the order they registered.
func (r *Registry) All() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Kind, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.kinds[id])
	}
	return out
}

// Check is a write's cells (by field id) as its fields store them: each
// coerced by its kind; a new row's fields it leaves out at their defaults; and
// for a complete write, every required field given. A cell of no field the
// table has is kept as it is (a field deleted since the writer read the
// table: today's rule, which a read never shows). Cells of kinds that aren't
// stored are left to the caller, which works them out or links them. It
// returns every cell it can't store, as CellErrors.
func (r *Registry) Check(fields []*model.Field, cells map[string]any, in Input) (map[string]any, error) {
	out := make(map[string]any, len(cells))
	for k, v := range cells {
		out[k] = v
	}
	var errs CellErrors
	for _, f := range fields {
		id := f.Id.String()
		k, known := r.Lookup(f.Type)
		if !known || !k.Stored {
			continue
		}
		v, given := out[id]
		if given {
			v = asJSON(v)
		}
		if !given && in.New && len(f.Default) > 0 {
			if d := decode(f.Default); !IsEmpty(d) {
				v, given = d, true
			}
		}
		if !given || IsEmpty(v) {
			if in.Complete && f.Required {
				errs = append(errs, CellError{Field: id, Msg: fmt.Sprintf("%q is required", f.Name)})
			}
			continue
		}
		if was, ok := in.Unchanged[id]; ok && reflect.DeepEqual(was, v) {
			continue
		}
		if k.Coerce == nil {
			out[id] = v
			continue
		}
		stored, err := k.Coerce(f, v, in)
		if err != nil {
			errs = append(errs, CellError{Field: id, Msg: err.Error()})
			continue
		}
		out[id] = stored
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

// CheckDefault is a value as a field's default stores it (nil for none), or
// why the field can't start new rows with it.
func (r *Registry) CheckDefault(f *model.Field, v any) (any, error) {
	if IsEmpty(v) {
		return nil, nil
	}
	k, known := r.Lookup(f.Type)
	if !known || !k.Stored || k.NoDefault || k.Coerce == nil {
		return nil, fmt.Errorf("%q can't have a default value", f.Name)
	}
	return k.Coerce(f, v, Input{})
}

// asJSON is a value as JSON gives one: what a writer in the server hands over
// typed ([]string from an import, an int) read as a request's body is, so
// every writer's value is checked alike. Pure.
func asJSON(v any) any {
	switch v.(type) {
	case nil, string, bool, float64, json.Number, []any, map[string]any:
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// decode is a stored JSON value as a write gives one.
func decode(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}
