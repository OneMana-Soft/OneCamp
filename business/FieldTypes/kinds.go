package business

// Today's twelve kinds: what each stores, how what a writer sends becomes
// that, and how a cell of another kind becomes one when a field's type
// changes (the rules W0 gave conversions, TAB-BUG-9, which a write's coercion
// follows too: what a type change would make of a value is what a write of it
// stores).

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// labelOf writes a number for a label: in full, as people write numbers,
// except from 1e21 up, which is 1e+21 as the web app shows it, and below
// 1e-30. Written in full, one of those takes over 32 characters, and 1e308
// takes 309: a list of them grouped 5,000 rows deep took a minute. Pure.
func labelOf(f float64) string {
	if a := math.Abs(f); a >= 1e21 || (a != 0 && a < 1e-30) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Labels is LabelsWith numbers written in full. Pure.
func Labels(v any) []string { return LabelsWith(v, labelOf) }

// --------------------------------------------------------------- conversion

// ConvertCell is a cell of a field changing to kind to: its value then, and
// whether it is kept as it is, converted, or cleared (cleared values; a
// cleared cell is nil). Text becomes a number as whoever changes the type
// writes numbers (signs). A checkbox reads as the grid shows it: ticked "Yes",
// unticked no value, which is nothing to clear. Pure.
func ConvertCell(to string, v any, signs formula.NumberSigns) (out any, kept bool, cleared int) {
	if b, ok := v.(bool); ok && to != model.FieldCheckbox {
		if !b {
			return nil, false, 0
		}
		out, _, cleared := ConvertCell(to, "Yes", signs)
		return out, false, cleared
	}
	k, ok := Kinds.Lookup(to)
	if !ok || k.Convert == nil {
		return nil, false, 1
	}
	return k.Convert(v, signs)
}

func toNumber(v any, signs formula.NumberSigns) (any, bool, int) {
	if n, ok := v.(float64); ok {
		return n, true, 0
	}
	if s, ok := v.(string); ok {
		if n, ok := formula.ParseNumber(s, signs); ok {
			return n, false, 0
		}
	}
	return nil, false, 1
}

func toCheckbox(v any, _ formula.NumberSigns) (any, bool, int) {
	if b, ok := v.(bool); ok {
		return b, true, 0
	}
	if b, ok := truth(fmt.Sprint(v)); ok && b {
		return true, false, 0
	}
	return nil, false, 1
}

func toDate(v any, _ formula.NumberSigns) (any, bool, int) {
	if s, ok := v.(string); ok {
		if d, ok := formula.ParseDate(s); ok {
			day := d.Time.Format("2006-01-02")
			return day, day == strings.TrimSpace(s), 0
		}
	}
	return nil, false, 1
}

func toMultiSelect(v any, _ formula.NumberSigns) (any, bool, int) {
	if list, ok := v.([]any); ok {
		return list, true, 0
	}
	if labels := Labels(v); len(labels) > 0 {
		return []any{strings.Join(labels, ", ")}, false, 0
	}
	return nil, false, 1
}

func toSelect(v any, _ formula.NumberSigns) (any, bool, int) {
	if s, ok := v.(string); ok {
		return s, true, 0
	}
	if labels := Labels(v); len(labels) > 0 {
		if _, isList := v.([]any); isList {
			return labels[0], false, len(labels) - 1
		}
		return labels[0], false, 0
	}
	return nil, false, 1
}

func toText(v any, _ formula.NumberSigns) (any, bool, int) {
	if s, ok := v.(string); ok {
		return s, true, 0
	}
	if labels := Labels(v); len(labels) > 0 {
		return strings.Join(labels, ", "), false, 0
	}
	return nil, false, 1
}

// truth reads text as a box ticked or not: "true", "yes", "1" and "✓" tick
// it, "false", "no", "0" and nothing leave it unticked. ok is false for
// anything else. Pure.
func truth(s string) (ticked, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "1", "✓":
		return true, true
	case "false", "no", "0", "":
		return false, true
	}
	return false, false
}

// ------------------------------------------------------------------ writing

// quoted is a value as a refusal quotes it: short, so a 100 KiB cell doesn't
// fill a message.
func quoted(v any) string {
	var s string
	if str, ok := v.(string); ok {
		s = str
	} else {
		b, _ := json.Marshal(v)
		s = string(b)
	}
	if r := []rune(s); len(r) > maxQuoted {
		s = string(r[:maxQuoted]) + "…"
	}
	return strconv.Quote(s)
}

// maxQuoted is how much of a refused value a refusal quotes.
const maxQuoted = 80

func coerceText(f *model.Field, v any, _ Input) (any, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case map[string]any:
		return nil, fmt.Errorf("%q takes text", f.Name)
	}
	// What the field's type change would make of it: a ticked box "Yes", a
	// list its labels.
	out, _, cleared := ConvertCell(f.Type, v, formula.PlainSigns)
	if cleared > 0 {
		return nil, fmt.Errorf("%q takes text", f.Name)
	}
	return out, nil
}

func coerceNumber(f *model.Field, v any, in Input) (any, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case json.Number:
		if n, err := x.Float64(); err == nil {
			return n, nil
		}
	case string:
		if n, ok := formula.ParseNumber(x, in.signs()); ok {
			return n, nil
		}
	}
	return nil, fmt.Errorf("%q takes a number: %s isn't one", f.Name, quoted(v))
}

func coerceCheckbox(f *model.Field, v any, _ Input) (any, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case float64:
		if x == 0 || x == 1 {
			return x == 1, nil
		}
	case string:
		if b, ok := truth(x); ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%q is ticked or not: %s is neither", f.Name, quoted(v))
}

func coerceDate(f *model.Field, v any, _ Input) (any, error) {
	if s, ok := v.(string); ok {
		if d, ok := formula.ParseDate(s); ok {
			return d.Time.Format("2006-01-02"), nil
		}
	}
	return nil, fmt.Errorf("%q takes a date, such as 2026-10-16: %s isn't one", f.Name, quoted(v))
}

// option is the label of the option a value names, by its label or its id.
func option(opts []Option, v string) (string, bool) {
	for _, o := range opts {
		if o.Label == v {
			return o.Label, true
		}
	}
	for _, o := range opts {
		if o.ID == v {
			return o.Label, true
		}
	}
	return "", false
}

// noOption is a refusal of a value that isn't one of a field's options, which
// names them (the first ten).
func noOption(f *model.Field, opts []Option, v any) error {
	if len(opts) == 0 {
		return fmt.Errorf("%q has no options yet: add %s to its options first", f.Name, quoted(v))
	}
	names := make([]string, 0, 10)
	for i, o := range opts {
		if i == 10 {
			names = append(names, fmt.Sprintf("%d more", len(opts)-10))
			break
		}
		names = append(names, o.Label)
	}
	return fmt.Errorf("%q has no option %s: its options are %s", f.Name, quoted(v), strings.Join(names, ", "))
}

func coerceSelect(f *model.Field, v any, _ Input) (any, error) {
	opts := OptionsOf(f)
	if s, ok := v.(string); ok {
		if label, ok := option(opts, s); ok {
			return label, nil
		}
	}
	return nil, noOption(f, opts, v)
}

func coerceMultiSelect(f *model.Field, v any, _ Input) (any, error) {
	opts := OptionsOf(f)
	items, isList := v.([]any)
	if !isList {
		items = []any{v}
	}
	out := make([]any, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		s, ok := it.(string)
		label, found := option(opts, s)
		if !ok || !found {
			return nil, noOption(f, opts, it)
		}
		if !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out, nil
}

func coerceRelation(f *model.Field, v any, _ Input) (any, error) {
	target := RelationTarget(f)
	refs, bad := ParseItems(v, target)
	if bad == "" {
		return refs, nil
	}
	kinds := ItemKinds[target]
	if target == ItemAny {
		kinds = "docs, tasks, boards, projects or people"
	}
	if r := []rune(bad); len(r) > maxQuoted {
		bad = string(r[:maxQuoted]) + "…"
	}
	return nil, fmt.Errorf("%q links to %s by id and type: %s isn't one", f.Name, kinds, bad)
}
