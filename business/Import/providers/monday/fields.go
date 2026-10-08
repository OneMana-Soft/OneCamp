package monday

// Board columns that aren't one of a task's own (status, priority, owner,
// dates and tags: pickColumns) become the project's custom fields when
// OneCamp has a kind of field for them: another status column a select,
// a dropdown a multi-select, numbers (money when the unit is a currency),
// people, dates, checkboxes, links and text. A column whose value a field
// carries whole leaves the description; everything else stays there, as
// before, so nothing a board showed is lost.
//
// Columns come with the board, their settings with them (API 2025-10 on:
// `settings`, a JSON object; `settings_str` is gone from 2026-07):
//
//	status    {"labels":[{"id":1,"label":"Done","color":"done_green","index":1,"is_deactivated":false}]}
//	dropdown  {"labels":[{"id":1,"label":"Marketing","is_deactivated":false}]}
//	numbers   {"unit":{"symbol":"$","custom_unit":"","direction":"left"}}
//
// The older shapes (status labels as an {"0":"Working on it"} map with
// hex labels_colors, dropdown labels with "name") are read too.

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// mondayColumn is one of a board's columns.
type mondayColumn struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Type     string          `json:"type"`
	Settings json.RawMessage `json:"settings"`
}

// asValue is the column as pickColumns reads an item's value of it, so a
// board's picks are its items'.
func (c mondayColumn) asValue() mondayColumnValue {
	return mondayColumnValue{ID: c.ID, Type: c.Type, Column: &struct {
		Title string `json:"title"`
	}{Title: c.Title}}
}

// fieldColumns is a board's columns that are custom fields: in the board's
// order, and by column id.
type fieldColumns struct {
	list     []importProvider.SourceField
	byColumn map[string]importProvider.SourceField
	// retired holds each dropdown's deactivated labels by id: the column no
	// longer offers them, but items still hold them.
	retired map[string][]importProvider.SourceOption
}

func fieldsOfBoard(b mondayBoard) fieldColumns {
	cvs := make([]mondayColumnValue, len(b.Columns))
	for i, c := range b.Columns {
		cvs[i] = c.asValue()
	}
	pk := pickColumns(cvs)
	// A date column that's neither the start nor the due date is the due
	// date of a board without one; beside a due date or a timeline it's a
	// field of its own.
	if pk.otherDate != nil && (pk.dueDate != nil || pk.timeline != nil) {
		pk.used[pk.otherDate.ID] = false
	}
	out := fieldColumns{byColumn: map[string]importProvider.SourceField{}, retired: map[string][]importProvider.SourceOption{}}
	for _, c := range b.Columns {
		if pk.used[c.ID] {
			continue
		}
		if f, ok := columnField(c); ok {
			out.list = append(out.list, f)
			out.byColumn[c.ID] = f
			if f.Type == importProvider.FieldMultiSelect {
				_, out.retired[c.ID] = dropdownLabels(settingsOf(c.Settings))
			}
		}
	}
	return out
}

// columnKind is a column type with monday's older names folded in.
func columnKind(t string) string {
	switch t = (mondayColumnValue{Type: t}).kind(); t {
	case "numeric":
		return "numbers"
	case "boolean":
		return "checkbox"
	case "long-text":
		return "long_text"
	}
	return t
}

// columnField is a column as a custom field, if OneCamp has a kind for it.
func columnField(c mondayColumn) (importProvider.SourceField, bool) {
	f := importProvider.SourceField{SourceID: c.ID, Name: strings.TrimSpace(c.Title)}
	if f.Name == "" {
		return f, false
	}
	settings := settingsOf(c.Settings)
	switch columnKind(c.Type) {
	case "status":
		f.Type, f.Options = importProvider.FieldSelect, statusOptions(settings)
	case "dropdown":
		f.Type, f.Options = importProvider.FieldMultiSelect, dropdownOptions(settings)
	case "people":
		f.Type = importProvider.FieldPerson
	case "numbers":
		f.Type = importProvider.FieldNumber
		if cur := unitCurrency(settings); cur != "" {
			f.Type, f.Currency = importProvider.FieldMoney, cur
		}
	case "rating":
		f.Type = importProvider.FieldNumber
	case "date":
		f.Type = importProvider.FieldDate
	case "checkbox":
		f.Type = importProvider.FieldCheckbox
	case "link":
		f.Type = importProvider.FieldURL
	case "text", "long_text", "email", "phone", "country", "location", "hour", "week":
		f.Type = importProvider.FieldText
	default:
		return f, false
	}
	return f, true
}

// columnSettings is the part of a column's settings fields need.
type columnSettings struct {
	Labels       json.RawMessage `json:"labels"`
	LabelsColors map[string]struct {
		Color string `json:"color"`
	} `json:"labels_colors"`
	Positions map[string]int `json:"labels_positions_v2"`
	Unit      *struct {
		Symbol     string `json:"symbol"`
		CustomUnit string `json:"custom_unit"`
	} `json:"unit"`
}

// settingsOf reads a column's settings, an object or (as some clients pass
// a JSON scalar on) a string holding one.
func settingsOf(raw json.RawMessage) columnSettings {
	var s columnSettings
	if len(raw) > 0 && raw[0] == '"' {
		var inner string
		if json.Unmarshal(raw, &inner) != nil {
			return s
		}
		raw = json.RawMessage(inner)
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

// labelSetting is one status or dropdown label in the current settings.
type labelSetting struct {
	ID          json.RawMessage `json:"id"`
	Label       string          `json:"label"`
	Name        string          `json:"name"`
	Color       string          `json:"color"`
	Index       *int            `json:"index"`
	Deactivated bool            `json:"is_deactivated"`
}

// statusKey is how a status label is matched: an item's value is read from
// its text, the label itself, since monday's index and id can differ.
func statusKey(label string) string { return strings.ToLower(cleanLabel(label)) }

func statusOptions(s columnSettings) []importProvider.SourceOption {
	type ranked struct {
		at  int
		opt importProvider.SourceOption
	}
	var all []ranked
	add := func(at int, label, color string) {
		if label = cleanLabel(label); label != "" {
			all = append(all, ranked{at, importProvider.SourceOption{SourceID: statusKey(label), Label: label, Color: mondayColor(color)}})
		}
	}
	var list []labelSetting
	var byIndex map[string]string
	switch {
	case json.Unmarshal(s.Labels, &list) == nil:
		for i, l := range list {
			if !l.Deactivated {
				at := i
				if l.Index != nil {
					at = *l.Index
				}
				add(at, l.Label, l.Color)
			}
		}
	case json.Unmarshal(s.Labels, &byIndex) == nil:
		for key, label := range byIndex {
			at, err := strconv.Atoi(key)
			if p, ok := s.Positions[key]; ok {
				at = p
			} else if err != nil {
				at = len(byIndex)
			}
			add(at, label, s.LabelsColors[key].Color)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at < all[j].at })
	out := make([]importProvider.SourceOption, len(all))
	for i, r := range all {
		out[i] = r.opt
	}
	return out
}

func dropdownOptions(s columnSettings) []importProvider.SourceOption {
	active, _ := dropdownLabels(s)
	return active
}

// dropdownLabels is a dropdown's labels as options: the ones it offers, and
// the ones it no longer does.
func dropdownLabels(s columnSettings) (active, retired []importProvider.SourceOption) {
	var list []labelSetting
	if json.Unmarshal(s.Labels, &list) != nil {
		return nil, nil
	}
	for _, l := range list {
		label := strings.TrimSpace(l.Label)
		if label == "" {
			label = strings.TrimSpace(l.Name)
		}
		id := importProvider.IDString(l.ID)
		if id == "" || label == "" {
			continue
		}
		o := importProvider.SourceOption{SourceID: id, Label: label}
		if l.Deactivated {
			retired = append(retired, o)
		} else {
			active = append(active, o)
		}
	}
	return active, retired
}

// unitCurrency is the currency a numbers column's unit is, if it's one.
func unitCurrency(s columnSettings) string {
	if s.Unit == nil {
		return ""
	}
	for _, u := range []string{s.Unit.Symbol, s.Unit.CustomUnit} {
		u = strings.TrimSpace(u)
		if sign, ok := unitNames[strings.ToLower(u)]; ok {
			u = sign
		}
		if cur := importProvider.CurrencyOf(u); cur != "" {
			return cur
		}
	}
	return ""
}

// unitNames are monday's names for the currency units it offers.
var unitNames = map[string]string{"dollar": "$", "euro": "€", "pound": "£", "yen": "¥", "rupee": "₹"}

// mondayColors are monday's label colour names, as palette colours.
var mondayColors = map[string]string{
	"done_green": "emerald", "working_orange": "amber", "stuck_red": "red", "american_gray": "slate",
	"aquamarine": "teal", "berry": "rose", "blackish": "slate", "bright_blue": "blue", "bright_green": "lime",
	"brown": "amber", "bubble": "pink", "chili_blue": "blue", "coffee": "amber", "dark_blue": "blue",
	"dark_indigo": "indigo", "dark_orange": "orange", "dark_purple": "purple", "dark_red": "red",
	"egg_yolk": "yellow", "explosive": "slate", "grass_green": "green", "indigo": "indigo", "lavender": "violet",
	"lilac": "violet", "lipstick": "rose", "navy": "indigo", "orchid": "pink", "peach": "orange", "pecan": "amber",
	"purple": "purple", "river": "sky", "royal": "blue", "saladish": "lime", "sky": "sky", "sofia_pink": "pink",
	"steel": "slate", "sunset": "orange", "tan": "amber", "teal": "teal", "winter": "slate",
}

// mondayColor is a label's colour, named (current settings) or hex (older).
func mondayColor(c string) string {
	if p, ok := mondayColors[strings.ToLower(c)]; ok {
		return p
	}
	return importProvider.PaletteColor(c)
}

// fieldValues is an item's values of its board's fields, and which columns
// they carry whole (so the description can leave them out).
func fieldValues(item mondayItem, fields fieldColumns) ([]importProvider.SourceFieldValue, map[string]bool) {
	var out []importProvider.SourceFieldValue
	carried := map[string]bool{}
	for _, cv := range item.ColumnValues {
		f, ok := fields.byColumn[cv.ID]
		if !ok {
			continue
		}
		v, whole := fieldValue(cv, f)
		if v == nil {
			continue
		}
		// A label the column no longer offers is still this item's: it comes
		// with the value, and the field gains it.
		switch x := v.(type) {
		case string:
			if f.Type == importProvider.FieldSelect && !hasOption(f, x) {
				f.Options = append(f.Options[:len(f.Options):len(f.Options)], importProvider.SourceOption{SourceID: x, Label: cleanLabel(cv.text())})
			}
		case []string:
			for _, id := range x {
				if hasOption(f, id) {
					continue
				}
				for _, o := range fields.retired[cv.ID] {
					if o.SourceID == id {
						f.Options = append(f.Options[:len(f.Options):len(f.Options)], o)
					}
				}
			}
		}
		out = append(out, importProvider.SourceFieldValue{Field: f, Value: v, Text: cv.text(), InDescription: !whole})
		if whole {
			carried[cv.ID] = true
		}
	}
	return out, carried
}

func hasOption(f importProvider.SourceField, id string) bool {
	for _, o := range f.Options {
		if o.SourceID == id {
			return true
		}
	}
	return false
}

// fieldValue is a column value as its field holds it, nil when there's none
// to read; whole is false when the field holds only part of it (the first
// of several people).
func fieldValue(cv mondayColumnValue, f importProvider.SourceField) (v any, whole bool) {
	text := cv.text()
	obj := map[string]json.RawMessage{}
	if b := cv.valueJSON(); b != nil {
		_ = json.Unmarshal(b, &obj)
	}
	str := func(key string) string {
		var s string
		if json.Unmarshal(obj[key], &s) == nil {
			return strings.TrimSpace(s)
		}
		return importProvider.IDString(obj[key])
	}
	switch f.Type {
	case importProvider.FieldSelect:
		if text == "" {
			return nil, false
		}
		return statusKey(text), true
	case importProvider.FieldMultiSelect:
		var ids []string
		var raw []json.RawMessage
		if json.Unmarshal(obj["ids"], &raw) == nil {
			for _, r := range raw {
				if id := importProvider.IDString(r); id != "" {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) == 0 {
			for _, name := range splitList(text) {
				for _, o := range f.Options {
					if strings.EqualFold(o.Label, name) {
						ids = append(ids, o.SourceID)
					}
				}
			}
		}
		if len(ids) == 0 {
			return nil, false
		}
		return ids, true
	case importProvider.FieldPerson:
		people := parsePeopleValue(cv)
		if len(people) == 0 {
			return nil, false
		}
		return people[0], len(people) == 1
	case importProvider.FieldNumber, importProvider.FieldMoney:
		for _, s := range []string{str("rating"), unquote(cv.valueJSON()), text} {
			if n, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), 64); err == nil {
				return n, true
			}
		}
		return nil, false
	case importProvider.FieldDate:
		d := str("date")
		if d == "" {
			d = firstField(text)
		}
		if parseDay(d) == nil {
			return nil, false
		}
		return d, true
	case importProvider.FieldCheckbox:
		if str("checked") == "true" || string(obj["checked"]) == "true" || strings.EqualFold(text, "v") {
			return true, true
		}
		return nil, false
	case importProvider.FieldURL:
		if u := str("url"); u != "" {
			return u, true
		}
		if text == "" {
			return nil, false
		}
		// The text is "label - url" when the link has a label.
		parts := strings.Split(text, " - ")
		return strings.TrimSpace(parts[len(parts)-1]), true
	case importProvider.FieldText:
		if e := str("email"); e != "" {
			return e, true
		}
		if text == "" {
			return nil, false
		}
		return text, true
	}
	return nil, false
}

// unquote is a JSON string's content (a numbers column's value is "\"12.5\"").
func unquote(b []byte) string {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	return string(b)
}
