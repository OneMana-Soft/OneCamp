package airtable

// Airtable's field types as OneCamp's kinds, and its cells as those kinds
// keep them. Each type goes to the nearest kind: what has no kind yet
// comes across as text and is said in the import's notes, never dropped
// without a word.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// mapping is one base's schema read for the import: which table each field
// is in, and each field's name, for formulas that name fields by id.
type mapping struct {
	tableOf map[string]string
	nameOf  map[string]string
	typeOf  map[string]string
}

func newMapping(s *schemaOf) *mapping {
	m := &mapping{tableOf: map[string]string{}, nameOf: map[string]string{}, typeOf: map[string]string{}}
	for _, t := range s.Tables {
		for _, f := range t.Fields {
			m.tableOf[f.ID] = t.ID
			m.nameOf[f.ID] = f.Name
			m.typeOf[f.ID] = f.Type
		}
	}
	return m
}

// fieldRef is a field named by its id in a formula: {fldXXXXXXXXXXXXXX}.
var fieldRef = regexp.MustCompile(`\{(fld[A-Za-z0-9]{14})\}`)

// formulaText is a formula with its fields named as OneCamp names them,
// {Name}; ok is false when it names a field the base doesn't have.
func (m *mapping) formulaText(src string) (string, bool) {
	ok := true
	out := fieldRef.ReplaceAllStringFunc(src, func(ref string) string {
		name, found := m.nameOf[ref[1:len(ref)-1]]
		if !found {
			ok = false
			return ref
		}
		return "{" + name + "}"
	})
	return out, ok
}

type choice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type options struct {
	Choices                 []choice `json:"choices"`
	LinkedTableID           string   `json:"linkedTableId"`
	InverseLinkFieldID      string   `json:"inverseLinkFieldId"`
	PrefersSingleRecordLink bool     `json:"prefersSingleRecordLink"`
	Max                     float64  `json:"max"`
	Formula                 string   `json:"formula"`
	RecordLinkFieldID       string   `json:"recordLinkFieldId"`
	FieldIDInLinkedTable    string   `json:"fieldIdInLinkedTable"`
	TimeZone                string   `json:"timeZone"`
	Result                  *struct {
		Type string `json:"type"`
	} `json:"result"`
}

func optionsOf(f atField) options {
	var o options
	_ = json.Unmarshal(f.Options, &o)
	return o
}

func selectConfig(o options) map[string]any {
	opts := make([]map[string]any, 0, len(o.Choices))
	for _, c := range o.Choices {
		opts = append(opts, map[string]any{"id": c.ID, "label": c.Name})
	}
	return map[string]any{"options": opts}
}

// valueKind is the stored kind a worked-out field's values are kept as when
// it can't be worked out here (by what Airtable says it gives).
func valueKind(o options) string {
	if o.Result != nil {
		switch o.Result.Type {
		case "number", "currency", "percent", "rating", "duration", "count", "autoNumber":
			return model.FieldNumber
		case "checkbox":
			return model.FieldCheckbox
		case "date", "dateTime":
			return model.FieldDate
		}
	}
	return model.FieldText
}

// field is an Airtable field as OneCamp's, and a note when it comes across
// as less than it was.
func (m *mapping) field(f atField) (dataTableBusiness.ImportField, string) {
	o := optionsOf(f)
	out := dataTableBusiness.ImportField{Key: f.ID, Name: f.Name, Description: f.Description}
	switch f.Type {
	case "singleLineText", "barcode":
		out.Type = model.FieldText
	case "multilineText", "richText", "aiText":
		out.Type = model.FieldLongText
	case "email":
		out.Type = model.FieldEmail
	case "url":
		out.Type = model.FieldURL
	case "phoneNumber":
		out.Type = model.FieldPhone
	case "number", "currency", "percent":
		out.Type = model.FieldNumber
	case "autoNumber":
		// Its numbers are Airtable's, kept as they were; new rows have none.
		out.Type = model.FieldNumber
	case "rating":
		out.Type = model.FieldRating
		if o.Max >= 1 && o.Max <= 10 {
			out.Config = map[string]any{"max": o.Max}
		}
	case "duration":
		out.Type = model.FieldDuration
	case "checkbox":
		out.Type = model.FieldCheckbox
	case "date", "dateTime", "createdTime", "lastModifiedTime":
		out.Type = model.FieldDate
	case "singleSelect", "externalSyncSource":
		out.Type, out.Config = model.FieldSelect, selectConfig(o)
	case "multipleSelects":
		out.Type, out.Config = model.FieldMultiSelect, selectConfig(o)
	case "singleCollaborator", "createdBy", "lastModifiedBy":
		out.Type, out.Config = model.FieldPeople, map[string]any{"one": true}
	case "multipleCollaborators":
		out.Type = model.FieldPeople
	case "multipleRecordLinks":
		out.Type, out.LinksTo, out.Pair = model.FieldRelation, o.LinkedTableID, o.InverseLinkFieldID
	case "formula":
		text, ok := m.formulaText(o.Formula)
		out.Type, out.Formula, out.Fallback = model.FieldFormula, text, valueKind(o)
		if !ok {
			out.Formula = ""
		}
	case "rollup":
		// Airtable's API doesn't say how a rollup adds up: its values stay.
		out.Type, out.Through, out.Of, out.Fallback = model.FieldRollup, o.RecordLinkFieldID, o.FieldIDInLinkedTable, valueKind(o)
	case "multipleLookupValues":
		out.Type, out.Through, out.Of, out.Aggregate, out.Fallback = model.FieldRollup, o.RecordLinkFieldID, o.FieldIDInLinkedTable, "list", model.FieldText
	case "count":
		out.Type, out.Through, out.Aggregate, out.Fallback = model.FieldRollup, o.RecordLinkFieldID, "count", model.FieldNumber
	case "multipleAttachments":
		out.Type = model.FieldAttachment
	case "button":
		return out, fmt.Sprintf("%q is a button, which has nothing to bring.", f.Name)
	default:
		out.Type = model.FieldText
		return out, fmt.Sprintf("%q is an Airtable %s field, which comes across as text.", f.Name, f.Type)
	}
	return out, ""
}

// table is an Airtable table as OneCamp's (its rows read later), its
// primary field first.
func (m *mapping) table(t atTable) dataTableBusiness.ImportTable {
	out := dataTableBusiness.ImportTable{Key: t.ID, Name: t.Name, Description: t.Description}
	var rest []dataTableBusiness.ImportField
	for _, f := range t.Fields {
		fi, _ := m.field(f)
		if fi.Type == "" {
			continue
		}
		if f.ID == t.PrimaryFieldID {
			out.Fields = append([]dataTableBusiness.ImportField{fi}, out.Fields...)
		} else {
			rest = append(rest, fi)
		}
	}
	out.Fields = append(out.Fields, rest...)
	for _, v := range t.Views {
		out.Views = append(out.Views, dataTableBusiness.ImportView{Name: v.Name, Type: viewType(v.Type)})
	}
	return out
}

// Notes is what a base's schema brings as less than it was, by field.
func (m *mapping) Notes(t atTable) map[string]string {
	out := map[string]string{}
	for _, f := range t.Fields {
		if _, note := m.field(f); note != "" {
			out[f.ID] = note
		}
	}
	return out
}

// viewType is an Airtable view's type as OneCamp's: kanban is a board; a
// calendar, a gallery, a list, a timeline and a Gantt keep their layouts; and
// what OneCamp has no layout for yet (a form) is a grid.
func viewType(t string) string {
	switch t {
	case "kanban":
		return "board"
	case "calendar", "gallery", "list", "timeline", "gantt":
		return t
	}
	return "grid"
}

// rows are records as OneCamp's rows.
func (m *mapping) rows(t atTable, recs []atRecord) []dataTableBusiness.ImportRow {
	out := make([]dataTableBusiness.ImportRow, 0, len(recs))
	for _, r := range recs {
		row := dataTableBusiness.ImportRow{Key: r.ID, Cells: map[string]any{}}
		for _, f := range t.Fields {
			raw, ok := r.Fields[f.ID]
			if !ok {
				if f.Type == "createdTime" {
					raw, _ = json.Marshal(r.CreatedTime)
				} else {
					continue
				}
			}
			if f.Type == "multipleRecordLinks" {
				var ids []string
				if json.Unmarshal(raw, &ids) == nil && len(ids) > 0 {
					if row.Links == nil {
						row.Links = map[string][]string{}
					}
					row.Links[f.ID] = ids
				}
				continue
			}
			if v := cellOf(f, optionsOf(f), raw); v != nil {
				row.Cells[f.ID] = v
			}
		}
		out = append(out, row)
	}
	return out
}

// cellOf is an Airtable cell as its OneCamp kind keeps it; nil for none.
func cellOf(f atField, o options, raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil || v == nil {
		return nil
	}
	switch f.Type {
	case "barcode":
		if b, ok := v.(map[string]any); ok {
			v, _ = b["text"].(string)
		}
	case "aiText":
		if b, ok := v.(map[string]any); ok {
			v, _ = b["value"].(string)
		}
	case "dateTime", "createdTime", "lastModifiedTime":
		s, _ := v.(string)
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil
		}
		// A day where the field's time zone says (UTC when it names none).
		if loc, err := time.LoadLocation(o.TimeZone); err == nil && o.TimeZone != "" && o.TimeZone != "client" {
			t = t.In(loc)
		}
		return t.Format("2006-01-02")
	case "singleCollaborator", "createdBy", "lastModifiedBy":
		if p := personOf(v); p != nil {
			return []any{*p}
		}
		return nil
	case "multipleCollaborators":
		list, _ := v.([]any)
		var out []any
		for _, x := range list {
			if p := personOf(x); p != nil {
				out = append(out, *p)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case "multipleAttachments":
		// Each file by its address, which Airtable signs for a few hours:
		// the tables stage fetches it as the row is written.
		list, _ := v.([]any)
		var files []any
		for _, x := range list {
			a, ok := x.(map[string]any)
			if !ok {
				continue
			}
			id, _ := a["id"].(string)
			addr, _ := a["url"].(string)
			name, _ := a["filename"].(string)
			mime, _ := a["type"].(string)
			size, _ := a["size"].(float64)
			if addr == "" {
				continue
			}
			if name == "" {
				name = "File"
			}
			files = append(files, dataTableBusiness.ImportFile{Name: name, Mime: mime, Size: int64(size),
				From: importProvider.SourceAttachment{SourceID: id, Name: name, URL: addr, Mime: mime, Size: int64(size)}})
		}
		if len(files) == 0 {
			return nil
		}
		return files
	case "multipleLookupValues", "rollup", "formula":
		// Kept as values when it can't be worked out here.
		return valueOf(v)
	}
	return valueOf(v)
}

// valueOf is a plain cell: text, a number, a tick, a list of labels; a list
// of anything else as its words.
func valueOf(v any) any {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return x
	case float64, bool:
		return x
	case []any:
		var labels []any
		var words []string
		for _, e := range x {
			switch y := e.(type) {
			case string:
				labels = append(labels, y)
				words = append(words, y)
			case float64:
				words = append(words, fmt.Sprint(y))
			case map[string]any:
				if n, _ := y["name"].(string); n != "" {
					words = append(words, n)
				}
			}
		}
		if len(labels) == len(x) && len(labels) > 0 {
			return labels
		}
		if len(words) == 0 {
			return nil
		}
		return strings.Join(words, ", ")
	case map[string]any:
		if n, _ := x["name"].(string); n != "" {
			return n
		}
	}
	return nil
}

func personOf(v any) *importProvider.PersonRef {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	email, _ := m["email"].(string)
	name, _ := m["name"].(string)
	if strings.TrimSpace(email) == "" {
		return nil
	}
	return &importProvider.PersonRef{Email: strings.ToLower(strings.TrimSpace(email)), Name: name}
}
