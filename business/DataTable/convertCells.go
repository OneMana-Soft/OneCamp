package business

// A field's cells when its type changes.
//
// Changing a column's type stored the new type and left every cell as it was
// (TAB-BUG-9), while the menu promised to clear what didn't fit: text turned
// into checkboxes showed every filled cell ticked, select to multi-select hid
// every value, and a chart and a filter on the column disagreed. Now the cells
// are converted in the field update's transaction, over every row, by
// convertCell, and a dry run (PreviewConversion) counts what it would do.
// Formulas, rollups and relations work their cells out, and keep them.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/akashc777/OneCamp/business/DataTable/formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// ConvertCounts is what converting a field's cells does: cells kept as they
// are, cells converted, and values cleared (a multi-select's items past its
// first, made a select, are each one).
type ConvertCounts struct {
	Kept      int `json:"kept"`
	Converted int `json:"converted"`
	Cleared   int `json:"cleared"`
}

// textual is the field types whose cells are text.
var textual = map[string]bool{model.FieldText: true, model.FieldURL: true, model.FieldEmail: true, model.FieldPerson: true}

// typeChangeProblem is why field was can't become type to, or nil: a link's
// cells are named for each reader on every read, and as another kind of
// field they would be read as they are stored. The update and its dry run
// both ask it.
func typeChangeProblem(was *model.Field, to string) error {
	if to == model.FieldRelation {
		return nil
	}
	if _, isLink := linkOf(was); isLink {
		return fmt.Errorf("A link to another table can't become another kind of field; delete it instead")
	}
	if _, isItems := itemsOf(was); isItems {
		return fmt.Errorf("A link to docs, tasks, boards, projects or people can't become another kind of field; delete it instead")
	}
	return nil
}

// converts is whether a field's cells are converted from type from to to.
func converts(from, to string) bool {
	stored := func(t string) bool {
		return t != model.FieldFormula && t != model.FieldRollup && t != model.FieldRelation
	}
	return from != to && stored(from) && stored(to)
}

// isEmpty is whether a cell holds nothing.
func isEmpty(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []interface{}:
		return len(x) == 0
	}
	return false
}

// convertCell is a cell of a field changing to type to: its value then, and
// whether it is kept, converted or cleared (cleared values; a cleared cell
// is nil). Text becomes a number as whoever changes the type writes numbers
// (signs): read with its commas dropped, "1,5" was 15 for good, and "NaN"
// failed the whole change.
func convertCell(to string, v interface{}, signs formula.NumberSigns) (out interface{}, kept bool, cleared int) {
	// A checkbox reads as the grid shows it: ticked "Yes", unticked no value,
	// which is nothing to clear. Every unticked row read "false".
	if b, ok := v.(bool); ok && to != model.FieldCheckbox {
		if !b {
			return nil, false, 0
		}
		out, _, cleared := convertCell(to, "Yes", signs)
		return out, false, cleared
	}
	switch {
	case to == model.FieldNumber:
		if n, ok := v.(float64); ok {
			return n, true, 0
		}
		if s, ok := v.(string); ok {
			if n, ok := formula.ParseNumber(s, signs); ok {
				return n, false, 0
			}
		}
	case to == model.FieldCheckbox:
		if b, ok := v.(bool); ok {
			return b, true, 0
		}
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(v))) {
		case "true", "yes", "1", "✓":
			return true, false, 0
		}
	case to == model.FieldDate:
		if s, ok := v.(string); ok {
			if d, ok := formula.ParseDate(s); ok {
				day := d.Time.Format("2006-01-02")
				return day, day == strings.TrimSpace(s), 0
			}
		}
	case to == model.FieldMultiSelect:
		if list, ok := v.([]interface{}); ok {
			return list, true, 0
		}
		if labels := cellLabels(v); len(labels) > 0 {
			return []interface{}{strings.Join(labels, ", ")}, false, 0
		}
	case to == model.FieldSelect:
		if s, ok := v.(string); ok {
			return s, true, 0
		}
		if labels := cellLabels(v); len(labels) > 0 {
			if _, isList := v.([]interface{}); isList {
				return labels[0], false, len(labels) - 1
			}
			return labels[0], false, 0
		}
	case textual[to]:
		if s, ok := v.(string); ok {
			return s, true, 0
		}
		if labels := cellLabels(v); len(labels) > 0 {
			return strings.Join(labels, ", "), false, 0
		}
	}
	return nil, false, 1
}

// conversion converts a field's cells (by row) to its type, in f, reading
// numbers in signs, and counts what it does. A select's or a multi-select's
// labels not among its options are added to them, in alphabetical order. It
// returns the cells that change.
func conversion(f *model.Field, cells map[uuid.UUID]json.RawMessage, signs formula.NumberSigns) (map[uuid.UUID]json.RawMessage, ConvertCounts, error) {
	var counts ConvertCounts
	out := map[uuid.UUID]json.RawMessage{}
	labels := map[string]bool{}
	for id, raw := range cells {
		var v interface{}
		if json.Unmarshal(raw, &v) != nil || isEmpty(v) {
			continue
		}
		to, kept, cleared := convertCell(f.Type, v, signs)
		counts.Cleared += cleared
		switch {
		case to == nil:
			out[id] = json.RawMessage("null")
			continue
		case kept:
			counts.Kept++
		default:
			counts.Converted++
			b, err := json.Marshal(to)
			if err != nil {
				return nil, counts, err
			}
			out[id] = b
		}
		if hasOptions(f.Type) {
			list, _ := to.([]interface{})
			if s, ok := to.(string); ok {
				list = []interface{}{s}
			}
			for _, it := range list {
				if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
					labels[s] = true
				}
			}
		}
	}
	if len(labels) == 0 {
		return out, counts, nil
	}
	cfg := map[string]interface{}{}
	_ = json.Unmarshal([]byte(f.Config), &cfg)
	list := optionsIn(cfg)
	for _, o := range list {
		delete(labels, optionLabel(o))
	}
	opts := make([]interface{}, 0, len(list)+len(labels))
	for _, o := range list {
		opts = append(opts, o)
	}
	added := make([]string, 0, len(labels))
	for s := range labels {
		added = append(added, s)
	}
	sort.Strings(added)
	for _, s := range added {
		opts = append(opts, map[string]interface{}{"label": s})
	}
	cfg["options"] = opts
	planOptions(nil, cfg, nil, false)
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, counts, err
	}
	f.Config = string(b)
	return out, counts, nil
}

// PreviewConversion counts what changing a field to type to would do to its
// cells, over every row, reading numbers as the asker writes them (signs),
// without changing anything (manage only): what the column menu says before
// it asks.
func PreviewConversion(ctx context.Context, tableID, fieldID uuid.UUID, to string, signs formula.NumberSigns, actor Actor) (*ConvertCounts, error) {
	if _, err := loadManageable(ctx, tableID, actor); err != nil {
		return nil, err
	}
	signs, ok := signs.Checked()
	if !ok {
		return nil, errNumberSigns
	}
	fields, err := model.ListFields(ctx, tableID)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	var was *model.Field
	for _, f := range fields {
		if f.Id == fieldID {
			was = f
		}
	}
	if was == nil {
		return nil, errFieldNotFound
	}
	if !model.ValidFieldType(to) {
		return nil, fmt.Errorf("invalid field type")
	}
	if err := typeChangeProblem(was, to); err != nil {
		return nil, err
	}
	counts := &ConvertCounts{}
	if !converts(was.Type, to) {
		return counts, nil
	}
	cells, err := model.FieldCells(ctx, tableID, fieldID)
	if err != nil {
		return nil, fmt.Errorf("failed to load rows")
	}
	into := *was
	into.Type = to
	_, *counts, err = conversion(&into, cells, signs)
	return counts, err
}
