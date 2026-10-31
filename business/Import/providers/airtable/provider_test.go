package airtable

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// Each Airtable type becomes the nearest OneCamp kind, with its options;
// what has no kind yet says so.
func TestAirtableFieldsBecomeTheNearestKind(t *testing.T) {
	m := &mapping{nameOf: map[string]string{"fldAAAAAAAAAAAAAA": "Price", "fldBBBBBBBBBBBBBB": "Qty"}}
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := []struct {
		f        atField
		kind     string
		note     bool
		check    func(f dataTableBusiness.ImportField) bool
		describe string
	}{
		{atField{ID: "f1", Name: "Name", Type: "singleLineText"}, model.FieldText, false, nil, "text"},
		{atField{ID: "f2", Name: "Notes", Type: "richText"}, model.FieldLongText, false, nil, "rich text as long text"},
		{atField{ID: "f3", Name: "Stage", Type: "singleSelect", Options: raw(`{"choices":[{"id":"selA","name":"Todo","color":"blueLight2"},{"id":"selB","name":"Done"}]}`)}, model.FieldSelect, false,
			func(f dataTableBusiness.ImportField) bool {
				return reflect.DeepEqual(f.Config, map[string]any{"options": []map[string]any{{"id": "selA", "label": "Todo"}, {"id": "selB", "label": "Done"}}})
			}, "select with its options"},
		{atField{ID: "f4", Name: "Stars", Type: "rating", Options: raw(`{"max":10}`)}, model.FieldRating, false,
			func(f dataTableBusiness.ImportField) bool { return f.Config["max"] == 10.0 }, "rating out of 10"},
		{atField{ID: "f5", Name: "Owner", Type: "singleCollaborator"}, model.FieldPeople, false,
			func(f dataTableBusiness.ImportField) bool { return f.Config["one"] == true }, "one person"},
		{atField{ID: "f6", Name: "Vendor", Type: "multipleRecordLinks", Options: raw(`{"linkedTableId":"tblV","inverseLinkFieldId":"fldO"}`)}, model.FieldRelation, false,
			func(f dataTableBusiness.ImportField) bool { return f.LinksTo == "tblV" && f.Pair == "fldO" }, "a link and its pair"},
		{atField{ID: "f7", Name: "Total", Type: "formula", Options: raw(`{"formula":"{fldAAAAAAAAAAAAAA} * {fldBBBBBBBBBBBBBB}","result":{"type":"number"}}`)}, model.FieldFormula, false,
			func(f dataTableBusiness.ImportField) bool {
				return f.Formula == "{Price} * {Qty}" && f.Fallback == model.FieldNumber
			}, "a formula, its fields by name"},
		{atField{ID: "f8", Name: "Spend", Type: "rollup", Options: raw(`{"recordLinkFieldId":"f6","fieldIdInLinkedTable":"fldX","result":{"type":"currency"}}`)}, model.FieldRollup, false,
			func(f dataTableBusiness.ImportField) bool {
				return f.Through == "f6" && f.Of == "fldX" && f.Aggregate == "" && f.Fallback == model.FieldNumber
			}, "a rollup, kept as values"},
		{atField{ID: "f9", Name: "Cities", Type: "multipleLookupValues", Options: raw(`{"recordLinkFieldId":"f6","fieldIdInLinkedTable":"fldC"}`)}, model.FieldRollup, false,
			func(f dataTableBusiness.ImportField) bool { return f.Aggregate == "list" }, "a lookup as a list"},
		{atField{ID: "f10", Name: "Orders", Type: "count", Options: raw(`{"recordLinkFieldId":"f6"}`)}, model.FieldRollup, false,
			func(f dataTableBusiness.ImportField) bool { return f.Aggregate == "count" }, "a count"},
		{atField{ID: "f11", Name: "Files", Type: "multipleAttachments"}, model.FieldAttachment, false, nil, "files as files"},
		{atField{ID: "f12", Name: "Go", Type: "button"}, "", true, nil, "a button, left out and said"},
		{atField{ID: "f13", Name: "Due", Type: "dateTime"}, model.FieldDate, false, nil, "a moment as a date"},
	}
	for _, c := range cases {
		f, note := m.field(c.f)
		if f.Type != c.kind || (note != "") != c.note || (c.check != nil && !c.check(f)) {
			t.Errorf("%s: %+v, note %q", c.describe, f, note)
		}
	}
	if _, ok := m.formulaText("{fldZZZZZZZZZZZZZZ} + 1"); ok {
		t.Error("a formula naming a field the base doesn't have read as fine")
	}
}

// Cells are kept as their kind keeps them.
func TestAirtableCellsAsTheirKindKeepsThem(t *testing.T) {
	cases := []struct {
		f    atField
		raw  string
		want any
	}{
		{atField{Type: "singleLineText"}, `"  Booth "`, "  Booth "},
		{atField{Type: "number"}, `12.5`, 12.5},
		{atField{Type: "checkbox"}, `true`, true},
		{atField{Type: "multipleSelects"}, `["hot","big"]`, []any{"hot", "big"}},
		{atField{Type: "barcode"}, `{"text":"0123","type":"upce"}`, "0123"},
		{atField{Type: "dateTime", Options: json.RawMessage(`{"timeZone":"Asia/Kolkata"}`)}, `"2026-10-18T20:00:00.000Z"`, "2026-10-19"},
		{atField{Type: "singleCollaborator"}, `{"id":"usr1","email":"Maya@Example.com","name":"Maya"}`, []any{importProvider.PersonRef{Email: "maya@example.com", Name: "Maya"}}},
		{atField{Type: "multipleAttachments"}, `[{"id":"att1","url":"https://x","filename":"plan.pdf","type":"application/pdf","size":2048},{"filename":"no address.jpg"}]`,
			[]any{dataTableBusiness.ImportFile{Name: "plan.pdf", Mime: "application/pdf", Size: 2048,
				From: importProvider.SourceAttachment{SourceID: "att1", Name: "plan.pdf", URL: "https://x", Mime: "application/pdf", Size: 2048}}}},
		{atField{Type: "multipleLookupValues"}, `["Pune","Goa"]`, []any{"Pune", "Goa"}},
		{atField{Type: "singleLineText"}, `""`, nil},
	}
	for _, c := range cases {
		got := cellOf(c.f, optionsOf(c.f), json.RawMessage(c.raw))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s: %#v, want %#v", c.f.Type, c.raw, got, c.want)
		}
	}
}

// A base read through the API: its tables, the primary field first, its
// views as OneCamp's, its records a page at a time with their links, and
// what comes across as less than it was in the notes.
func TestAirtableBaseThroughTheAPI(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pat123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/meta/bases/appB/tables":
			_, _ = w.Write([]byte(`{"tables":[{"id":"tblO","name":"Orders","primaryFieldId":"fldT","fields":[
				{"id":"fldF","name":"Files","type":"multipleAttachments"},
				{"id":"fldT","name":"Title","type":"singleLineText"},
				{"id":"fldV","name":"Vendor","type":"multipleRecordLinks","options":{"linkedTableId":"tblV","inverseLinkFieldId":"fldO"}}],
				"views":[{"id":"viw1","name":"Board","type":"kanban"},{"id":"viw2","name":"Pics","type":"gallery"}]}]}`))
		case r.URL.Path == "/appB/tblO":
			if r.URL.Query().Get("returnFieldsByFieldId") != "true" {
				t.Errorf("records asked without field ids: %s", r.URL.RawQuery)
			}
			pages++
			if r.URL.Query().Get("offset") == "" {
				_, _ = w.Write([]byte(`{"records":[{"id":"rec1","fields":{"fldT":"Booth","fldV":["recA"]}}],"offset":"p2"}`))
			} else {
				_, _ = w.Write([]byte(`{"records":[{"id":"rec2","fields":{"fldT":"Banners","fldF":[{"id":"att9","url":"https://dl.airtable/a.png","filename":"a.png"}]}}]}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	was := apiBase
	apiBase = srv.URL
	defer func() { apiBase = was }()

	p := New()
	s, err := p.schema(context.Background(), "pat123", "appB")
	if err != nil {
		t.Fatal(err)
	}
	m := newMapping(s)
	tb := m.table(s.Tables[0])
	if tb.Fields[0].Key != "fldT" || len(tb.Views) != 2 || tb.Views[0].Type != "board" || tb.Views[1].Type != "gallery" {
		t.Errorf("table: %+v", tb)
	}
	var rows []dataTableBusiness.ImportRow
	err = p.records(context.Background(), "pat123", "appB", "tblO", func(rs []atRecord) error {
		rows = append(rows, m.rows(s.Tables[0], rs)...)
		return nil
	})
	if err != nil || pages != 2 || len(rows) != 2 {
		t.Fatalf("records: %v, %d pages, %d rows", err, pages, len(rows))
	}
	files, _ := rows[1].Cells["fldF"].([]any)
	if !reflect.DeepEqual(rows[0].Links, map[string][]string{"fldV": {"recA"}}) || len(files) != 1 || files[0].(dataTableBusiness.ImportFile).Name != "a.png" {
		t.Errorf("rows: %+v", rows)
	}
	if notes := m.Notes(s.Tables[0]); len(notes) != 0 {
		t.Errorf("notes: %v", notes)
	}
	if err := p.get(context.Background(), "wrong", "/meta/bases/appB/tables", nil, &schemaOf{}); !errorsIsTokenRejected(err) {
		t.Errorf("a wrong token: %v", err)
	}
}

func errorsIsTokenRejected(err error) bool {
	return errors.Is(err, importProvider.ErrTokenRejected)
}
