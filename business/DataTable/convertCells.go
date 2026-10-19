package business

// A field's cells when its type changes.
//
// Changing a column's type stored the new type and left every cell as it was
// (TAB-BUG-9), while the menu promised to clear what didn't fit: text turned
// into checkboxes showed every filled cell ticked, select to multi-select hid
// every value, and a chart and a filter on the column disagreed. Now the cells
// are converted in the field update's transaction, over every row, by
// each kind's rule (business/FieldTypes ConvertCell), and a dry run
// (PreviewConversion) counts what it would do.
// Formulas, rollups and relations work their cells out, and keep them.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
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
		if json.Unmarshal(raw, &v) != nil || fieldTypes.IsEmpty(v) {
			continue
		}
		to, kept, cleared := fieldTypes.ConvertCell(f.Type, v, signs)
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
	list := fieldTypes.Options(cfg)
	for _, o := range list {
		delete(labels, fieldTypes.OptionLabel(o))
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
	if !fieldTypes.Kinds.Valid(to) {
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
