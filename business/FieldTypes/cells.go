package business

// How a cell reads, whatever its kind: the labels it shows and whether it
// holds anything. One reading for every place that needs one: conversions
// between kinds, totals and charts, formulas reading a cell as text, and
// what a link's chip is called.

import (
	"encoding/json"
	"strconv"
	"strings"

	formula "github.com/akashc777/OneCamp/business/Formula"
)

// IsEmpty is whether a cell holds nothing: no value, blank text, or an empty
// list. Pure.
func IsEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	}
	return false
}

// LabelsWith reduces a cell to the labels it shows, with numbers written by
// format: one for a value, one per item of a list (a multi-select, people,
// links), none for an empty cell. A link reads as its label, or failing one
// its name, value, title or id; one past those a cell shows (IsMoreRef), or
// one its reader can't open (IsHiddenRef), names nothing. Pure.
func LabelsWith(v any, format func(float64) string) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []string{t}
	case bool:
		return []string{strconv.FormatBool(t)}
	case float64:
		return []string{format(t)}
	case json.Number:
		return []string{t.String()}
	case []any:
		var out []string
		for _, e := range t {
			switch el := e.(type) {
			case string:
				if strings.TrimSpace(el) != "" {
					out = append(out, el)
				}
			case map[string]any:
				if lbl := RefLabel(el); lbl != "" && !IsMoreRef(el) && !IsHiddenRef(el) {
					out = append(out, lbl)
				}
			case float64:
				out = append(out, format(el))
			case bool:
				out = append(out, strconv.FormatBool(el))
			}
		}
		return out
	case map[string]any:
		if lbl := RefLabel(t); lbl != "" && !IsMoreRef(t) && !IsHiddenRef(t) {
			return []string{lbl}
		}
		return nil
	default:
		return nil
	}
}

// RefLabel is what a link reads as: its name (RefName), or failing one its
// id. Pure.
func RefLabel(m map[string]any) string {
	if s := RefName(m); s != "" {
		return s
	}
	if s, ok := m["id"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return ""
}

// RefName is the name a link shows: its label, or failing one its name,
// value or title; never its id, which only the app reads. Pure.
func RefName(m map[string]any) string {
	for _, k := range []string{"label", "name", "value", "title"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// IsMoreRef is whether a cell's item is a read's count of the links past
// those it shows ({"type": "more"}), not a link. Pure.
func IsMoreRef(item any) bool {
	m, ok := item.(map[string]any)
	return ok && m["type"] == "more"
}

// HasMoreRef is whether a link cell, as a read gives it, shows only some of
// its links (IsMoreRef): it has links, however few it names. Pure.
func HasMoreRef(cell any) bool {
	items, _ := cell.([]any)
	for _, it := range items {
		if IsMoreRef(it) {
			return true
		}
	}
	return false
}

// IsHiddenRef is whether a cell's item is a link to something its reader
// can't open ({"hidden": true}): a link, with no name to read. Pure.
func IsHiddenRef(item any) bool {
	m, ok := item.(map[string]any)
	return ok && m["hidden"] == true
}

// FormulaKindOf is how a field of kind fieldType reads in a formula: as its
// kind says, text for one nothing knows.
func FormulaKindOf(fieldType string) formula.Kind {
	if k, ok := Kinds.Lookup(fieldType); ok {
		return k.FormulaKind
	}
	return formula.KindText
}

// FormulaValue reads a cell of a field of kind fieldType for a formula, with the same helpers totals and
// filters use: a number field as a number, a date as a date, a checkbox as
// yes or no, and anything else as its labels ("Design, Launch" for a
// multi-select, a person's or a linked item's name).
func FormulaValue(fieldType string, raw interface{}) formula.Value {
	// By what its kind reads as (business/FieldTypes): a duration, a rating
	// and a row's number are numbers; when a row was made or changed, dates.
	switch FormulaKindOf(fieldType) {
	case formula.KindNumber:
		if f, ok := CellNumber(raw); ok {
			return formula.Number(f)
		}
		return formula.Blank()
	case formula.KindBool:
		b, _ := raw.(bool)
		return formula.Bool(b)
	case formula.KindDate:
		if s, ok := raw.(string); ok {
			if v, ok := formula.ParseDate(s); ok {
				return v
			}
		}
		return formula.Blank()
	default:
		// A number in a list reads as a number cell does (1e+21 from 1e21).
		labels := LabelsWith(raw, formula.FormatNumber)
		if _, isList := raw.([]interface{}); !isList {
			return formula.Text(strings.Join(labels, ", "))
		}
		// A list (a multi-select's options, people, linked items) is its
		// labels joined, as before, and its items for the ARRAY functions.
		// Every label is kept, a blank one too, so the text is as it was.
		items := make([]formula.Value, len(labels))
		for i, l := range labels {
			items[i] = formula.Text(l)
		}
		return formula.List(items)
	}
}

// CellNumber attempts to read a numeric value from a cell (number column, or a
// numeric string, read as formula.ParseNumber reads one in en-US's signs).
// Returns (value, true) only on success. Text was read by dropping its
// commas, so "1,5" summed as 15, and "NaN" as a number no answer could hold.
// Totals and formulas read cells so; a filter reads them as the grid does
// (rowFilters.go), where a number written as text is no number.
func CellNumber(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f, true
		}
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		return formula.ParseNumber(t, formula.PlainSigns)
	}
	return 0, false
}
