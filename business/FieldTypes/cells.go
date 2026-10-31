package business

// How a cell reads, whatever its kind: the labels it shows and whether it
// holds anything. One reading for every place that needs one: conversions
// between kinds, totals and charts, formulas reading a cell as text, and
// what a link's chip is called.

import (
	"encoding/json"
	"strconv"
	"strings"
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
