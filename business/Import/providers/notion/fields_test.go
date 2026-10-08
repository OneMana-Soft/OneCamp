package notion

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A database's properties as GET /v1/databases/{id} returns them.
const databaseJSON = `{"properties":{
  "Name":{"id":"title","name":"Name","type":"title","title":{}},
  "Status":{"id":"st","name":"Status","type":"status","status":{"options":[{"id":"s1","name":"Not started","color":"default"},{"id":"s2","name":"Done","color":"green"}]}},
  "Priority":{"id":"pr","name":"Priority","type":"select","select":{"options":[{"id":"p1","name":"High","color":"red"}]}},
  "Assignee":{"id":"as","name":"Assignee","type":"people","people":{}},
  "Due":{"id":"du","name":"Due","type":"date","date":{}},
  "Channel":{"id":"ch","name":"Channel","type":"select","select":{"options":[{"id":"c1","name":"Blog","color":"purple"},{"id":"c2","name":"Email","color":"blue"}]}},
  "Platforms":{"id":"pl","name":"Platforms","type":"multi_select","multi_select":{"options":[{"id":"m1","name":"iOS","color":"brown"},{"id":"m2","name":"Web","color":"gray"}]}},
  "Tags":{"id":"tg","name":"Tags","type":"multi_select","multi_select":{"options":[{"id":"t1","name":"launch"}]}},
  "Budget":{"id":"bu","name":"Budget","type":"number","number":{"format":"euro"}},
  "Effort":{"id":"ef","name":"Effort","type":"number","number":{"format":"number_with_commas"}},
  "Launch":{"id":"la","name":"Launch","type":"date","date":{}},
  "Reviewer":{"id":"rv","name":"Reviewer","type":"people","people":{}},
  "Signed off":{"id":"so","name":"Signed off","type":"checkbox","checkbox":{}},
  "Brief":{"id":"br","name":"Brief","type":"url","url":{}},
  "Contact":{"id":"co","name":"Contact","type":"email","email":{}},
  "Notes":{"id":"no","name":"Notes","type":"rich_text","rich_text":{}},
  "Cost":{"id":"fo","name":"Cost","type":"formula","formula":{}},
  "Related":{"id":"re","name":"Related","type":"relation","relation":{}}
}}`

// One row as POST /v1/databases/{id}/query returns it.
const pageJSON = `{"id":"page-1","properties":{
  "Name":{"id":"title","type":"title","title":[{"type":"text","plain_text":"Launch post"}]},
  "Status":{"id":"st","type":"status","status":{"id":"s2","name":"Done","color":"green"}},
  "Channel":{"id":"ch","type":"select","select":{"id":"c1","name":"Blog","color":"purple"}},
  "Platforms":{"id":"pl","type":"multi_select","multi_select":[{"id":"m1","name":"iOS"},{"id":"m2","name":"Web"}]},
  "Tags":{"id":"tg","type":"multi_select","multi_select":[{"id":"t1","name":"launch"}]},
  "Budget":{"id":"bu","type":"number","number":1250.5},
  "Effort":{"id":"ef","type":"number","number":null},
  "Launch":{"id":"la","type":"date","date":{"start":"2026-11-02T09:00:00.000+05:30","end":null}},
  "Reviewer":{"id":"rv","type":"people","people":[{"object":"user","id":"u-maya","name":"Maya"},{"object":"user","id":"u-ada","name":"Ada"}]},
  "Signed off":{"id":"so","type":"checkbox","checkbox":true},
  "Brief":{"id":"br","type":"url","url":"https://docs.example.com/b"},
  "Contact":{"id":"co","type":"email","email":null},
  "Notes":{"id":"no","type":"rich_text","rich_text":[{"type":"text","plain_text":"Short "},{"type":"text","plain_text":"and warm"}]},
  "Cost":{"id":"fo","type":"formula","formula":{"type":"number","number":42}}
}}`

func TestDatabaseFields(t *testing.T) {
	var db notionDatabase
	if err := json.Unmarshal([]byte(databaseJSON), &db); err != nil {
		t.Fatal(err)
	}
	s := schemaOf(db.Properties)
	var got []string
	for _, f := range s.fieldList {
		got = append(got, f.Name+":"+f.Type+f.Currency)
	}
	// Name, Status, Priority, Assignee and Due are the task's own; Tags are
	// its tags; the formula and the relation aren't brought across.
	want := "Brief:url Budget:moneyEUR Channel:select Contact:text Effort:number Launch:date Notes:text Platforms:multi_select Reviewer:person Signed off:checkbox"
	if strings.Join(got, " ") != want {
		t.Fatalf("fields:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if opts := s.fieldByName["Platforms"].Options; len(opts) != 2 || opts[0].SourceID != "m1" || opts[0].Color != "amber" || opts[1].Color != "slate" {
		t.Errorf("options by id, Notion's colours matched: %+v", opts)
	}
}

func TestPageFieldValues(t *testing.T) {
	var db notionDatabase
	var page notionPage
	if err := json.Unmarshal([]byte(databaseJSON), &db); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(pageJSON), &page); err != nil {
		t.Fatal(err)
	}
	s := schemaOf(db.Properties)
	got := map[string]any{}
	for _, v := range fieldValues(page.Properties, s.fieldByName) {
		got[v.Field.Name] = v.Value
	}
	want := map[string]any{
		"Channel": "c1", "Platforms": []string{"m1", "m2"}, "Budget": 1250.5,
		// The day as the page has it, whatever the time.
		"Launch": "2026-11-02", "Reviewer": "u-maya", "Signed off": true,
		"Brief": "https://docs.example.com/b", "Notes": "Short and warm",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values:\n got %v\nwant %v", got, want)
	}
	for _, v := range fieldValues(page.Properties, s.fieldByName) {
		if v.Field.Name == "Reviewer" && v.Text != "Maya, Ada" {
			t.Errorf("people come with their names: %+v", v)
		}
	}
}
