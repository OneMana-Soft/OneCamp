package clickup

// ClickUp's custom fields become the project's own fields: dropdowns
// (select), labels (multi-select), text, numbers, money, dates,
// checkboxes, links, people and ratings. Every task from /list/{id}/task
// carries each field the list has, with its type and options, and a value
// when it has one, so a list's fields are read from its tasks with no
// call of their own. Formulas, progress, relationships and the like,
// which ClickUp works out or links elsewhere, aren't brought across.
//
//	drop_down  value: the option's orderindex (an id on some workspaces)
//	labels     value: ["<option id>", …]
//	currency   value: 1250.5, in units; type_config.currency_type "USD"
//	date       value: "1730505600000", milliseconds
//	users      value: [{"id":123,…}]
//	checkbox   value: true or "true"
//	emoji      value: 3 (a rating)

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

type clickupField struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Type       string             `json:"type"`
	TypeConfig clickupFieldConfig `json:"type_config"`
	Value      json.RawMessage    `json:"value"`
}

type clickupFieldConfig struct {
	Options      []clickupFieldOption `json:"options"`
	CurrencyType string               `json:"currency_type"`
}

type clickupFieldOption struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`  // drop_down
	Label      string          `json:"label"` // labels
	Color      string          `json:"color"`
	OrderIndex json.RawMessage `json:"orderindex"`
}

// fieldOf is a ClickUp field as a OneCamp one, if it can be one.
func fieldOf(cf clickupField) (importProvider.SourceField, bool) {
	f := importProvider.SourceField{SourceID: cf.ID, Name: strings.TrimSpace(cf.Name)}
	if f.SourceID == "" || f.Name == "" {
		return f, false
	}
	switch cf.Type {
	case "drop_down", "labels":
		f.Type = importProvider.FieldSelect
		if cf.Type == "labels" {
			f.Type = importProvider.FieldMultiSelect
		}
		for _, o := range cf.TypeConfig.Options {
			label := o.Name
			if label == "" {
				label = o.Label
			}
			if o.ID != "" {
				f.Options = append(f.Options, importProvider.SourceOption{SourceID: o.ID, Label: label, Color: importProvider.PaletteColor(o.Color)})
			}
		}
	case "text", "short_text", "email", "phone", "location":
		f.Type = importProvider.FieldText
	case "number", "emoji":
		f.Type = importProvider.FieldNumber
	case "currency":
		f.Type, f.Currency = importProvider.FieldMoney, importProvider.CurrencyCode(cf.TypeConfig.CurrencyType)
	case "date":
		f.Type = importProvider.FieldDate
	case "checkbox":
		f.Type = importProvider.FieldCheckbox
	case "url":
		f.Type = importProvider.FieldURL
	case "users":
		f.Type = importProvider.FieldPerson
	default:
		return f, false
	}
	return f, true
}

// listFields is each list's fields, in the order its tasks first show them.
func listFields(tasks []clickupTask) map[string][]importProvider.SourceField {
	out := map[string][]importProvider.SourceField{}
	seen := map[string]bool{}
	for _, t := range tasks {
		for _, cf := range t.CustomFields {
			key := t.ListID + "\x1f" + cf.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			if f, ok := fieldOf(cf); ok {
				out[t.ListID] = append(out[t.ListID], f)
			}
		}
	}
	return out
}

// fieldValues is a task's values of its fields.
func fieldValues(cfs []clickupField) []importProvider.SourceFieldValue {
	var out []importProvider.SourceFieldValue
	for _, cf := range cfs {
		f, ok := fieldOf(cf)
		if !ok || len(cf.Value) == 0 || string(cf.Value) == "null" {
			continue
		}
		if v := fieldValue(cf); v != nil {
			out = append(out, importProvider.SourceFieldValue{Field: f, Value: v, Text: usernames(cf)})
		}
	}
	return out
}

func fieldValue(cf clickupField) any {
	raw := cf.Value
	switch cf.Type {
	case "drop_down":
		return dropdownOption(cf, raw)
	case "labels":
		var ids []string
		if json.Unmarshal(raw, &ids) != nil || len(ids) == 0 {
			return nil
		}
		return ids
	case "number", "emoji", "currency":
		if n, ok := number(raw); ok {
			return n
		}
	case "date":
		if n, ok := number(raw); ok && n > 0 {
			return dayOf(int64(n))
		}
	case "checkbox":
		if b, ok := boolean(raw); ok && b {
			return true
		}
	case "users":
		var people []struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(raw, &people) == nil && len(people) > 0 {
			if id := importProvider.IDString(people[0].ID); id != "" {
				return id
			}
		}
	case "location":
		var loc struct {
			Address string `json:"formatted_address"`
		}
		if json.Unmarshal(raw, &loc) == nil && strings.TrimSpace(loc.Address) != "" {
			return loc.Address
		}
	default:
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return nil
}

// usernames is the people a users field names, for when they themselves
// can't come across.
func usernames(cf clickupField) string {
	if cf.Type != "users" {
		return ""
	}
	var people []struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(cf.Value, &people) != nil {
		return ""
	}
	var names []string
	for _, p := range people {
		if p.Username != "" {
			names = append(names, p.Username)
		}
	}
	return strings.Join(names, ", ")
}

// dropdownOption is the id of the option a dropdown's value names, by its
// orderindex (a number, or a number in a string) or its id.
func dropdownOption(cf clickupField, raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		for _, o := range cf.TypeConfig.Options {
			if o.ID == s {
				return s
			}
		}
	}
	n, ok := number(raw)
	if !ok {
		return nil
	}
	for _, o := range cf.TypeConfig.Options {
		if i, ok := number(o.OrderIndex); ok && i == n {
			return o.ID
		}
	}
	return nil
}

// number reads a JSON number, or a number in a string (ClickUp sends both).
func number(raw json.RawMessage) (float64, bool) {
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n, !math.IsNaN(n)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func boolean(raw json.RawMessage) (bool, bool) {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		b, err := strconv.ParseBool(s)
		return b, err == nil
	}
	return false, false
}

// dayOf is the day a ClickUp date names. ClickUp keeps a date picked
// without a time (its default) at 04:00 where it was picked: from 14:00 UTC
// the day before (UTC+14) to 16:00 UTC that day (UTC-12). Nine hours on,
// every zone from UTC+13 to UTC-10 lands on its own day, wherever the
// person importing is. A date picked with a time late in the day, well west
// of UTC, can land on the next day; the value doesn't say which kind it is,
// and dates without a time are the common case. Pure.
func dayOf(ms int64) string {
	return time.UnixMilli(ms).UTC().Add(9 * time.Hour).Format("2006-01-02")
}
