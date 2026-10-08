package business

// Formula fields (business/DataTable/formula). A formula's values are worked
// out each time rows are read, never stored, so every reader (the grid, the
// guest link, the rows API, totals and a table's questions, AI columns) gets
// the same value, and one that reads TODAY() is never stale. A field's config
// stores the formula with fields named by id ({"formula": "{#<id>} * 2"}), so
// renaming a field doesn't break it; readers get it with names.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/DataTable/formula"
	"github.com/akashc777/OneCamp/helpers/topo"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

type zoneKey struct{}

// WithZone carries the time zone the request's reader is in (an IANA name,
// such as "Asia/Kolkata"), where a formula's TODAY() is. An empty or unknown
// zone leaves it at UTC, as Airtable's formulas default to.
func WithZone(ctx context.Context, tz string) context.Context {
	if tz == "" {
		return ctx
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, zoneKey{}, loc)
}

func zoneFrom(ctx context.Context) *time.Location {
	if loc, ok := ctx.Value(zoneKey{}).(*time.Location); ok {
		return loc
	}
	return time.UTC
}

// kindOf is how a field's cells read in a formula.
func kindOf(fieldType string) formula.Kind {
	switch fieldType {
	case model.FieldNumber:
		return formula.KindNumber
	case model.FieldDate:
		return formula.KindDate
	case model.FieldCheckbox:
		return formula.KindBool
	default:
		return formula.KindText
	}
}

// cellValue reads a cell for a formula, with the same helpers totals and
// filters use: a number field as a number, a date as a date, a checkbox as
// yes or no, and anything else as its labels ("Design, Launch" for a
// multi-select, a person's or a linked item's name).
func cellValue(fieldType string, raw interface{}) formula.Value {
	switch fieldType {
	case model.FieldNumber:
		if f, ok := cellNumber(raw); ok {
			return formula.Number(f)
		}
		return formula.Blank()
	case model.FieldCheckbox:
		b, _ := raw.(bool)
		return formula.Bool(b)
	case model.FieldDate:
		if s, ok := raw.(string); ok {
			if v, ok := formula.ParseDate(s); ok {
				return v
			}
		}
		return formula.Blank()
	default:
		// A number in a list reads as a number cell does (1e+21 from 1e21).
		return formula.Text(strings.Join(labelsWith(raw, formula.FormatNumber), ", "))
	}
}

// storedFormula is a formula field's formula as stored.
func storedFormula(f *model.Field) string {
	var cfg struct {
		Formula string `json:"formula"`
	}
	_ = json.Unmarshal([]byte(f.Config), &cfg)
	return cfg.Formula
}

// formulaInputs is a table's fields as formulas see them.
func formulaInputs(fields []*model.Field) []formula.Field {
	out := make([]formula.Field, 0, len(fields))
	for _, f := range fields {
		in := formula.Field{ID: f.Id.String(), Name: f.Name, Kind: kindOf(f.Type)}
		if f.Type == model.FieldFormula {
			in.IsFormula, in.Formula = true, storedFormula(f)
		}
		out = append(out, in)
	}
	return out
}

// withFormulas works out the table's formulas for rows (one read, sharing its
// budget) and writes each value into its row, where every reader finds a
// cell. A row's stored text is kept as it is and the values are added to it:
// only the cells formulas read are decoded. It returns the program, for the
// fields' result types.
func withFormulas(ctx context.Context, fields []*model.Field, rows []*model.Row) *formula.Program {
	p := formula.Compile(formulaInputs(fields))
	if p.Empty() || len(rows) == 0 {
		return p
	}
	types := make(map[string]string, len(fields))
	for _, f := range fields {
		types[f.Id.String()] = f.Type
	}
	now, loc := time.Now(), zoneFrom(ctx)
	for _, r := range rows {
		if r == nil {
			continue
		}
		cells, ok := rowMembers(r.Values)
		if !ok {
			cells, r.Values = canonicalMembers(r.Values)
		}
		at := make(map[string]int, len(cells))
		for i, m := range cells {
			at[m.key] = i // a key given twice reads as its last, as in encoding/json
		}
		out := p.Run(func(id string) (formula.Value, int) {
			i, ok := at[id]
			if !ok {
				return cellValue(types[id], nil), 0
			}
			raw := r.Values[cells[i].value:cells[i].end]
			var v interface{}
			_ = json.Unmarshal([]byte(raw), &v)
			return cellValue(types[id], v), formula.ReadCost(raw)
		}, now, loc)
		r.Values = withValues(r.Values, cells, out)
	}
	return p
}

// member is one of the cells in a row's stored text: its key, and where it
// lies, as `"key": value` from start to end, with the value from value.
type member struct {
	key               string
	start, value, end int
}

// rowMembers finds the cells in a row's stored text, a JSON object, without
// decoding them. ok is false when the text isn't one.
func rowMembers(s string) ([]member, bool) {
	i := skipSpace(s, 0)
	if i == len(s) || s[i] != '{' {
		return nil, false
	}
	var ms []member
	if i = skipSpace(s, i+1); i < len(s) && s[i] == '}' {
		return ms, skipSpace(s, i+1) == len(s)
	}
	for {
		if i == len(s) || s[i] != '"' {
			return nil, false
		}
		start := i
		end, ok := skipString(s, i)
		if !ok {
			return nil, false
		}
		key := s[i+1 : end-1]
		if strings.IndexByte(key, '\\') >= 0 && json.Unmarshal([]byte(s[i:end]), &key) != nil {
			return nil, false
		}
		if i = skipSpace(s, end); i == len(s) || s[i] != ':' {
			return nil, false
		}
		value := skipSpace(s, i+1)
		if i, ok = skipValue(s, value); !ok {
			return nil, false
		}
		ms = append(ms, member{key: key, start: start, value: value, end: i})
		if i = skipSpace(s, i); i == len(s) {
			return nil, false
		}
		switch s[i] {
		case ',':
			i = skipSpace(s, i+1)
		case '}':
			return ms, skipSpace(s, i+1) == len(s)
		default:
			return nil, false
		}
	}
}

// canonicalMembers is for text rowMembers can't read: written again by
// encoding/json, which every JSON object survives, and read from that; an
// empty object when it isn't one.
func canonicalMembers(s string) ([]member, string) {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &raw) == nil && raw != nil {
		if b, err := json.Marshal(raw); err == nil {
			if cells, ok := rowMembers(string(b)); ok {
				return cells, string(b)
			}
		}
	}
	return nil, "{}"
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

// skipString is where the JSON string opening at s[i] ends, after its quote.
func skipString(s string, i int) (int, bool) {
	for j := i + 1; j < len(s); {
		k := strings.IndexAny(s[j:], `"\`)
		if k < 0 {
			break
		}
		if j += k; s[j] == '\\' {
			j += 2
			continue
		}
		return j + 1, true
	}
	return 0, false
}

// skipValue is where the JSON value starting at s[i] ends.
func skipValue(s string, i int) (int, bool) {
	if i == len(s) {
		return 0, false
	}
	switch s[i] {
	case '"':
		return skipString(s, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(s); j++ {
			switch s[j] {
			case '"':
				end, ok := skipString(s, j)
				if !ok {
					return 0, false
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1, true
				}
			}
		}
		return 0, false
	}
	// A number, true, false or null runs to the next comma, brace or space.
	j := i
	for j < len(s) && s[j] != ',' && s[j] != '}' && s[j] != ']' && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != '\r' {
		j++
	}
	return j, j > i
}

// withValues is a row's stored text with the formulas' values: the stored
// cells as they were, less any left under a formula's id, then the values.
func withValues(s string, cells []member, out map[string]formula.Value) string {
	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.Grow(len(s) + 64*len(ids))
	b.WriteByte('{')
	n := 0
	next := func() {
		if n > 0 {
			b.WriteByte(',')
		}
		n++
	}
	for _, m := range cells {
		if _, isFormula := out[m.key]; !isFormula {
			next()
			b.WriteString(s[m.start:m.end])
		}
	}
	for _, id := range ids {
		j := out[id].JSON()
		if j == nil {
			continue
		}
		v, err := json.Marshal(j)
		if err != nil {
			continue
		}
		key, _ := json.Marshal(id)
		next()
		b.Write(key)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.String()
}

// presentFormulaFields gives each formula field's config as readers want it:
// the formula with fields by name, what it gives ("result": number, text,
// date or checkbox) and, when it can't be worked out, why ("error").
func presentFormulaFields(fields []*model.Field, p *formula.Program) {
	inputs := formulaInputs(fields)
	for _, f := range fields {
		if f.Type != model.FieldFormula {
			continue
		}
		cfg := map[string]interface{}{}
		_ = json.Unmarshal([]byte(f.Config), &cfg)
		cfg["formula"] = formula.Display(storedFormula(f), inputs)
		cfg["result"] = p.Kind(f.Id.String()).Name()
		delete(cfg, "error")
		if err := p.Err(f.Id.String()); err != nil {
			cfg["error"] = formula.Message(err)
		}
		if b, err := json.Marshal(cfg); err == nil {
			f.Config = string(b)
		}
	}
}

// readWithFormulas loads a table's fields and works out its formulas for
// rows: the rows API, totals and AI columns, which load rows on their own.
func readWithFormulas(ctx context.Context, tableID uuid.UUID, rows []*model.Row) []*model.Row {
	if len(rows) == 0 {
		return rows
	}
	fields, err := model.ListFields(ctx, tableID)
	if err == nil {
		withFormulas(ctx, fields, rows)
	}
	return rows
}

// withoutFormulaValues drops values for a table's formula fields from a row
// being written: they're worked out on each read, never stored, and the grid
// sends a row back with them in.
func withoutFormulaValues(ctx context.Context, tableID uuid.UUID, values map[string]interface{}) (map[string]interface{}, []*model.Field) {
	fields, err := model.ListFields(ctx, tableID)
	if err != nil {
		return values, nil
	}
	for _, f := range fields {
		if f.Type == model.FieldFormula {
			delete(values, f.Id.String())
		}
	}
	return values, fields
}

// maxFormulaFields is how many formula fields a table can have: every read
// works each one out for every row.
const maxFormulaFields = 100

// roomForFormula is why field self can't be a formula: the table already has
// as many as it can, not counting self.
func roomForFormula(fields []*model.Field, self uuid.UUID) error {
	n := 0
	for _, f := range fields {
		if f.Type == model.FieldFormula && f.Id != self {
			n++
		}
	}
	if n >= maxFormulaFields {
		return fmt.Errorf("A table can have at most %d formula fields", maxFormulaFields)
	}
	return nil
}

// draftID is how a formula check names the field being saved: its id, or
// "new" for one that isn't saved yet.
func draftID(self uuid.UUID) string {
	if self == uuid.Nil {
		return "new"
	}
	return self.String()
}

// formulaConfig checks a formula field's formula against the table's other
// fields, and returns the config to store: the formula with fields by id. self
// is the field being saved (uuid.Nil for a new one).
func formulaConfig(ctx context.Context, tableID, self uuid.UUID, cfg map[string]interface{}) (map[string]interface{}, error) {
	fields, err := model.ListFields(ctx, tableID)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	return formulaConfigWith(fields, self, cfg)
}

// formulaConfigWith is formulaConfig against the table's fields, loaded.
func formulaConfigWith(fields []*model.Field, self uuid.UUID, cfg map[string]interface{}) (map[string]interface{}, error) {
	if err := roomForFormula(fields, self); err != nil {
		return nil, err
	}
	src, _ := cfg["formula"].(string)
	inputs := formulaInputs(fields)
	if _, err := formula.Check(src, inputs, draftID(self)); err != nil {
		return nil, err
	}
	stored, err := formula.Canonical(src, inputs)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	for k, v := range cfg {
		out[k] = v
	}
	out["formula"] = stored
	// Worked out on each read; never stored.
	delete(out, "result")
	delete(out, "error")
	return out, nil
}

// formulasInOrder puts a template's formula fields in an order where each
// comes after the formulas it reads, so one pass adds them all. Formulas that
// read each other in a loop come last, and fail their check.
func formulasInOrder(fis []FieldInput) []FieldInput {
	byName := map[string]int{}
	for i, fi := range fis {
		name := strings.ToLower(strings.TrimSpace(fi.Name))
		if _, taken := byName[name]; !taken {
			byName[name] = i
		}
	}
	idx := make([]int, len(fis))
	for i := range fis {
		idx[i] = i
	}
	ordered, looped := topo.Order(idx, func(i int) []int {
		src, _ := fis[i].Config["formula"].(string)
		var reads []int
		for _, ref := range formula.Refs(src) {
			if j, ok := byName[strings.ToLower(ref)]; ok {
				reads = append(reads, j)
			}
		}
		return reads
	})
	out := make([]FieldInput, 0, len(fis))
	for _, i := range append(ordered, looped...) {
		out = append(out, fis[i])
	}
	return out
}

// FormulaPreview is a formula being written, checked against the table's
// fields: what it gives and its values in the first rows, or why it can't be
// read.
type FormulaPreview struct {
	Result string        `json:"result"`
	Error  string        `json:"error,omitempty"`
	Values []interface{} `json:"values"`
}

// previewRows is how many rows a preview works out.
const previewRows = 5

// PreviewFormula checks a formula for a field (uuid.Nil for a new one) on a
// table the actor may manage, without saving it.
func PreviewFormula(ctx context.Context, tableID, fieldID uuid.UUID, src string, actor Actor) (*FormulaPreview, error) {
	if _, err := loadManageable(ctx, tableID, actor); err != nil {
		return nil, err
	}
	fields, err := model.ListFields(ctx, tableID)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	if err := roomForFormula(fields, fieldID); err != nil {
		return &FormulaPreview{Result: formula.KindText.Name(), Error: err.Error(), Values: []interface{}{}}, nil
	}
	inputs := formulaInputs(fields)
	kind, err := formula.Check(src, inputs, draftID(fieldID))
	if err != nil {
		return &FormulaPreview{Result: kind.Name(), Error: err.Error(), Values: []interface{}{}}, nil
	}
	stored, err := formula.Canonical(src, inputs)
	if err != nil {
		return &FormulaPreview{Result: kind.Name(), Error: err.Error(), Values: []interface{}{}}, nil
	}
	// The draft stands in for the field, and runs on the first rows.
	draft := &model.Field{Id: uuid.New(), Name: "", Type: model.FieldFormula}
	if fieldID != uuid.Nil {
		draft.Id = fieldID
	}
	cfg, _ := json.Marshal(map[string]string{"formula": stored})
	draft.Config = string(cfg)
	with := make([]*model.Field, 0, len(fields)+1)
	for _, f := range fields {
		if f.Id != draft.Id {
			with = append(with, f)
		}
	}
	with = append(with, draft)
	rows, err := model.ListRows(ctx, tableID, previewRows, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to load rows")
	}
	withFormulas(ctx, with, rows)
	out := &FormulaPreview{Result: kind.Name(), Values: make([]interface{}, 0, len(rows))}
	for _, r := range rows {
		out.Values = append(out.Values, parseRowValues(r.Values)[draft.Id.String()])
	}
	return out, nil
}
