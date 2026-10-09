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

// kindOfField is how a field's cells read in a formula: a rollup by what it
// gives, any other field by its type.
func kindOfField(f *model.Field) formula.Kind {
	if cfg, ok := rollupOf(f); ok {
		if k, ok := rollupGives[cfg.Aggregate]; ok {
			return k
		}
		return formula.KindText
	}
	return kindOf(f.Type)
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
		in := formula.Field{ID: f.Id.String(), Name: f.Name, Kind: kindOfField(f)}
		if f.Type == model.FieldFormula {
			in.IsFormula, in.Formula = true, storedFormula(f)
		}
		out = append(out, in)
	}
	return out
}

// withComputed works out a page of rows' computed cells, as one read
// (computed.go): the labels of the rows they link to, the links other tables
// make to them, rollups and formulas. It writes them into each row's stored
// text, where every reader finds a cell, and returns what it found, for
// presentComputedFields.
func withComputed(ctx context.Context, fields []*model.Field, rows []*model.Row) *computed {
	c := planComputed(ctx, fields)
	c.apply(ctx, rows)
	return c
}

// withComputedFor is withComputed for only the computed cells of the fields
// ids names, and of those they read: a query's, which reads no others.
func withComputedFor(ctx context.Context, fields []*model.Field, rows []*model.Row, ids []string) {
	if len(ids) > 0 {
		planComputedFor(ctx, fields, computedNeeds(fields, ids)).apply(ctx, rows)
	}
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

// withValues is a row's stored text with its computed values (out, by field
// id, as JSON values; nil for none): the stored cells as they were, less any
// under a computed field's id, then the values.
func withValues(s string, cells []member, out map[string]interface{}) string {
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
		if _, computed := out[m.key]; !computed {
			next()
			b.WriteString(s[m.start:m.end])
		}
	}
	for _, id := range ids {
		j := out[id]
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

// readWithComputed loads a table's fields and works out its computed cells
// for rows: the rows API, totals and AI columns, which load rows on their own.
func readWithComputed(ctx context.Context, tableID uuid.UUID, rows []*model.Row) []*model.Row {
	if len(rows) == 0 {
		return rows
	}
	fields, err := model.ListFields(ctx, tableID)
	if err == nil {
		withComputed(ctx, fields, rows)
	}
	return rows
}

// stripComputed takes what each read works out out of a row being written,
// as the grid sends a row back with it in: formula and rollup values, and
// the cells of links to tables, which aren't stored in a row's values (a
// write sets them with takeLinks).
func stripComputed(fields []*model.Field, values map[string]interface{}) map[string]interface{} {
	for _, f := range fields {
		if _, isLink := linkOf(f); isLink || f.Type == model.FieldFormula || f.Type == model.FieldRollup {
			delete(values, f.Id.String())
		}
	}
	return values
}

// How many fields a table can have of each kind worked out on every read,
// for every row: formulas, rollups, and relations linking to tables (each
// reads another table).
var maxComputed = map[string]int{
	model.FieldFormula:  100,
	model.FieldRollup:   100,
	model.FieldRelation: 50,
}

// computedName is how the limit names each kind of field.
var computedName = map[string]string{
	model.FieldFormula:  "formula fields",
	model.FieldRollup:   "rollups",
	model.FieldRelation: "fields linking to tables",
}

// roomFor is why field self can't be of a kind worked out on every read
// (fieldType; for a relation, linking is whether it links to a table): the
// table already has as many as it can, not counting self.
func roomFor(fields []*model.Field, self uuid.UUID, fieldType string, linking bool) error {
	limit, ok := maxComputed[fieldType]
	if !ok || (fieldType == model.FieldRelation && !linking) {
		return nil
	}
	n := 0
	for _, f := range fields {
		if f.Id == self || f.Type != fieldType {
			continue
		}
		if _, isLink := linkOf(f); fieldType != model.FieldRelation || isLink {
			n++
		}
	}
	if n >= limit {
		return fmt.Errorf("A table can have at most %d %s", limit, computedName[fieldType])
	}
	return nil
}

// roomForFormula is roomFor a formula field.
func roomForFormula(fields []*model.Field, self uuid.UUID) error {
	return roomFor(fields, self, model.FieldFormula, false)
}

// draftID is how a formula check names the field being saved: its id, or
// "new" for one that isn't saved yet.
func draftID(self uuid.UUID) string {
	if self == uuid.Nil {
		return "new"
	}
	return self.String()
}

// formulaConfigWith checks a formula field's formula against the table's
// other fields, and returns the config to store: the formula with fields by
// id. self is the field being saved (uuid.Nil for a new one).
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
	withComputed(asViewer(ctx, actor), with, rows)
	out := &FormulaPreview{Result: kind.Name(), Values: make([]interface{}, 0, len(rows))}
	for _, r := range rows {
		out.Values = append(out.Values, parseRowValues(r.Values)[draft.Id.String()])
	}
	return out, nil
}
