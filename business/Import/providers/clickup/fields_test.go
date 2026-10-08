package clickup

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A task as /list/{id}/task returns it: every field of the list, a value
// where one is set.
const taskWithFields = `{"id":"t1","name":"Launch post","list":{"id":"L1"},"custom_fields":[
  {"id":"f1","name":"Stage","type":"drop_down","type_config":{"options":[
     {"id":"s0","name":"Draft","color":"#e50000","orderindex":0},{"id":"s1","name":"Live","color":"#1bbc9c","orderindex":"1"}]},"value":1},
  {"id":"f2","name":"Channels","type":"labels","type_config":{"options":[{"id":"c1","label":"Blog","color":"#7b68ee"},{"id":"c2","label":"Email"}]},"value":["c2","c1"]},
  {"id":"f3","name":"Budget","type":"currency","type_config":{"currency_type":"EUR","precision":2},"value":"1250.5"},
  {"id":"f4","name":"Launch","type":"date","type_config":{},"value":"1793572200000"},
  {"id":"f5","name":"Reviewer","type":"users","type_config":{},"value":[{"id":81,"username":"Maya"}]},
  {"id":"f6","name":"Signed off","type":"checkbox","type_config":{},"value":"true"},
  {"id":"f7","name":"Score","type":"emoji","type_config":{"count":5},"value":4},
  {"id":"f8","name":"Brief","type":"url","type_config":{},"value":"https://docs.example.com/b"},
  {"id":"f9","name":"Contact","type":"email","type_config":{},"value":"ada@example.com"},
  {"id":"f10","name":"Where","type":"location","type_config":{},"value":{"location":{"lat":1,"lng":2},"formatted_address":"Bengaluru"}},
  {"id":"f11","name":"Cost","type":"formula","type_config":{},"value":42},
  {"id":"f12","name":"Notes","type":"text","type_config":{}},
  {"id":"f13","name":"Old stage","type":"drop_down","type_config":{"options":[{"id":"x1","name":"A","orderindex":0}]},"value":"x1"}
]}`

func TestTaskFieldValues(t *testing.T) {
	var task clickupTask
	if err := json.Unmarshal([]byte(taskWithFields), &task); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, v := range fieldValues(task.CustomFields) {
		got[v.Field.Name] = v.Value
	}
	want := map[string]any{
		// orderindex 1 is Live, whether ClickUp sends it as 1 or "1".
		"Stage": "s1", "Channels": []string{"c2", "c1"}, "Budget": 1250.5,
		// 1793572200000 is 2026-11-02 04:00 in India (ClickUp's hour for a
		// date without a time), 22:30 the day before in UTC.
		"Launch": "2026-11-02", "Reviewer": "81", "Signed off": true, "Score": 4.0,
		"Brief": "https://docs.example.com/b", "Contact": "ada@example.com", "Where": "Bengaluru",
		"Old stage": "x1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values:\n got %v\nwant %v", got, want)
	}
	for _, v := range fieldValues(task.CustomFields) {
		if v.Field.Name == "Reviewer" && v.Text != "Maya" {
			t.Errorf("a person comes with their name: %+v", v)
		}
	}
}

func TestFieldOf(t *testing.T) {
	var task clickupTask
	if err := json.Unmarshal([]byte(taskWithFields), &task); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, cf := range task.CustomFields {
		if f, ok := fieldOf(cf); ok {
			kinds = append(kinds, f.Name+":"+f.Type+f.Currency)
		}
	}
	want := "Stage:select Channels:multi_select Budget:moneyEUR Launch:date Reviewer:person Signed off:checkbox Score:number Brief:url Contact:text Where:text Notes:text Old stage:select"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("kinds:\n got %s\nwant %s", strings.Join(kinds, " "), want)
	}
	stage, _ := fieldOf(task.CustomFields[0])
	if !reflect.DeepEqual(stage.Options, []importProvider.SourceOption{{SourceID: "s0", Label: "Draft", Color: "red"}, {SourceID: "s1", Label: "Live", Color: "teal"}}) {
		t.Errorf("options with their colours: %+v", stage.Options)
	}
	channels, _ := fieldOf(task.CustomFields[1])
	if channels.Options[0].Label != "Blog" || channels.Options[0].Color != "violet" {
		t.Errorf("a labels field names its options by label: %+v", channels.Options)
	}
}

func TestListFields(t *testing.T) {
	cf := func(id, name string) clickupField { return clickupField{ID: id, Name: name, Type: "text"} }
	tasks := []clickupTask{
		{ListID: "L1", CustomFields: []clickupField{cf("a", "A"), cf("b", "B")}},
		{ListID: "L2", CustomFields: []clickupField{cf("a", "A")}},
		{ListID: "L1", CustomFields: []clickupField{cf("b", "B"), cf("c", "C"), {ID: "f", Name: "Sum", Type: "formula"}}},
	}
	fields := listFields(tasks)
	names := func(fs []importProvider.SourceField) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Name)
		}
		return strings.Join(out, ",")
	}
	if names(fields["L1"]) != "A,B,C" || names(fields["L2"]) != "A" {
		t.Errorf("each list's fields once, in order: %v", fields)
	}
}

func TestDayOf(t *testing.T) {
	zones := []string{"Pacific/Auckland", "Asia/Kolkata", "Europe/London", "America/New_York", "America/Los_Angeles", "Pacific/Honolulu"}
	for _, zone := range zones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skip("no zone data")
		}
		// A date picked in ClickUp without a time, and one a client stored
		// at midnight.
		for _, hour := range []int{4, 0} {
			at := time.Date(2026, 11, 2, hour, 0, 0, 0, loc)
			if hour == 0 && zone == "Pacific/Auckland" {
				continue // midnight at UTC+13 is past the window; ClickUp itself uses 04:00
			}
			if got := dayOf(at.UnixMilli()); got != "2026-11-02" {
				t.Errorf("%02d:00 on 2 November in %s read as %s", hour, zone, got)
			}
		}
	}
}

func TestCurrencyKeptAsNamed(t *testing.T) {
	f, ok := fieldOf(clickupField{ID: "f", Name: "Fee", Type: "currency", TypeConfig: clickupFieldConfig{CurrencyType: "MUR"}})
	if !ok || f.Type != importProvider.FieldMoney || f.Currency != "MUR" {
		t.Errorf("a currency ClickUp names is the field's, however rare: %+v", f)
	}
}
