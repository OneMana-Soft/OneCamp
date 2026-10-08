package trello

// Trello's custom fields (the Custom Fields power-up) become the board's
// project's own fields: a dropdown is a select, and checkboxes, dates,
// numbers and text are what they say. The board brings its fields
// (customFields=true, or "customFields" in a JSON export) and each card
// its values (card_customFieldItems=true, or "customFieldItems"):
//
//	{"idCustomField":"f1","idValue":"o2"}                  a dropdown's option
//	{"idCustomField":"f2","value":{"checked":"true"}}
//	{"idCustomField":"f3","value":{"date":"2026-11-02T12:00:00.000Z"}}
//	{"idCustomField":"f4","value":{"number":"12.5"}}
//	{"idCustomField":"f5","value":{"text":"Acme"}}

import (
	"strconv"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

type trelloCustomField struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Options []struct {
		ID    string `json:"id"`
		Color string `json:"color"`
		Value struct {
			Text string `json:"text"`
		} `json:"value"`
	} `json:"options"`
}

type trelloCustomFieldItem struct {
	IDCustomField string `json:"idCustomField"`
	IDValue       string `json:"idValue"`
	Value         *struct {
		Checked string `json:"checked"`
		Date    string `json:"date"`
		Number  string `json:"number"`
		Text    string `json:"text"`
	} `json:"value"`
}

// boardFields is a board's custom fields, in the board's order, and by id.
func boardFields(defs []trelloCustomField) ([]importProvider.SourceField, map[string]importProvider.SourceField) {
	var list []importProvider.SourceField
	byID := map[string]importProvider.SourceField{}
	for _, d := range defs {
		f := importProvider.SourceField{SourceID: d.ID, Name: strings.TrimSpace(d.Name)}
		if f.SourceID == "" || f.Name == "" {
			continue
		}
		switch d.Type {
		case "list":
			f.Type = importProvider.FieldSelect
			for _, o := range d.Options {
				if o.ID != "" {
					f.Options = append(f.Options, importProvider.SourceOption{SourceID: o.ID, Label: o.Value.Text, Color: importProvider.PaletteColor(o.Color)})
				}
			}
		case "checkbox":
			f.Type = importProvider.FieldCheckbox
		case "date":
			f.Type = importProvider.FieldDate
		case "number":
			f.Type = importProvider.FieldNumber
		case "text":
			f.Type = importProvider.FieldText
		default:
			continue
		}
		list = append(list, f)
		byID[f.SourceID] = f
	}
	return list, byID
}

// cardFieldValues is a card's values of its board's fields.
func cardFieldValues(items []trelloCustomFieldItem, byID map[string]importProvider.SourceField) []importProvider.SourceFieldValue {
	var out []importProvider.SourceFieldValue
	for _, it := range items {
		f, ok := byID[it.IDCustomField]
		if !ok {
			continue
		}
		var v any
		switch f.Type {
		case importProvider.FieldSelect:
			if it.IDValue != "" {
				v = it.IDValue
			}
		case importProvider.FieldCheckbox:
			if it.Value != nil && it.Value.Checked == "true" {
				v = true
			}
		case importProvider.FieldDate:
			// A date field holds a moment; its day in UTC is the day it
			// names for a time picked during the day almost anywhere.
			if it.Value != nil {
				if t, err := time.Parse(time.RFC3339, it.Value.Date); err == nil {
					v = t.UTC().Format("2006-01-02")
				}
			}
		case importProvider.FieldNumber:
			if it.Value != nil {
				if n, err := strconv.ParseFloat(strings.TrimSpace(it.Value.Number), 64); err == nil {
					v = n
				}
			}
		case importProvider.FieldText:
			if it.Value != nil && strings.TrimSpace(it.Value.Text) != "" {
				v = it.Value.Text
			}
		}
		if v != nil {
			out = append(out, importProvider.SourceFieldValue{Field: f, Value: v})
		}
	}
	return out
}
