package monday

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

func col(id, typ, title, settings string) mondayColumn {
	c := mondayColumn{ID: id, Type: typ, Title: title}
	if settings != "" {
		c.Settings = json.RawMessage(settings)
	}
	return c
}

// A board as API 2026-07 returns it: settings are objects.
func roadmapBoard() mondayBoard {
	return mondayBoard{ID: "1", Columns: []mondayColumn{
		col("name", "name", "Name", ""),
		col("status", "status", "Status", `{"labels":[{"id":0,"label":"Working on it","color":"working_orange","index":0},{"id":1,"label":"Done","color":"done_green","index":1}]}`),
		col("approval", "status", "Approval", `{"labels":[
			{"id":3,"label":"Rejected","color":"stuck_red","index":2},
			{"id":1,"label":"Approved ✅","color":"done_green","index":0},
			{"id":7,"label":"Old","color":"egg_yolk","index":3,"is_deactivated":true},
			{"id":5,"label":"Waiting","color":"#c4c4c4","index":1}]}`),
		col("person", "people", "Owner", ""),
		col("reviewer", "people", "Reviewer", ""),
		col("priority", "status", "Priority", `{"labels":[{"id":0,"label":"High","index":0}]}`),
		col("platforms", "dropdown", "Platforms", `{"labels":[{"id":1,"label":"iOS"},{"id":2,"label":"Android"},{"id":3,"label":"Gone","is_deactivated":true}]}`),
		col("tags", "tags", "Tags", ""),
		col("budget", "numbers", "Budget", `{"unit":{"symbol":"$","custom_unit":"","direction":"left"}}`),
		col("hours", "numbers", "Hours", `{"unit":{"symbol":"custom","custom_unit":"hrs","direction":"right"}}`),
		col("rating", "rating", "Score", ""),
		col("date4", "date", "Due date", ""),
		col("launch", "date", "Launch", ""),
		col("signed", "checkbox", "Signed off", ""),
		col("brief", "link", "Brief", ""),
		col("email", "email", "Contact", ""),
		col("notes", "long_text", "Notes", ""),
		col("formula", "formula", "Cost", ""),
		col("files", "file", "Files", ""),
		col("blank", "text", "  ", ""),
	}}
}

func TestFieldsOfBoard(t *testing.T) {
	fields := fieldsOfBoard(roadmapBoard())
	var got []string
	for _, f := range fields.list {
		got = append(got, f.Name+":"+f.Type+f.Currency)
	}
	// Status, Owner, Priority, Tags and Due date are the task's own; the
	// formula, the files and the nameless column have no field kind.
	want := "Approval:select Reviewer:person Platforms:multi_select Budget:moneyUSD Hours:number Score:number Launch:date Signed off:checkbox Brief:url Contact:text Notes:text"
	if strings.Join(got, " ") != want {
		t.Fatalf("fields:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	approval := fields.byColumn["approval"]
	if !reflect.DeepEqual(approval.Options, []importProvider.SourceOption{
		{SourceID: "approved", Label: "Approved", Color: "emerald"},
		{SourceID: "waiting", Label: "Waiting", Color: "slate"},
		{SourceID: "rejected", Label: "Rejected", Color: "red"},
	}) {
		t.Errorf("labels in their order, emoji and deactivated ones left out, colours matched: %+v", approval.Options)
	}
	if opts := fields.byColumn["platforms"].Options; len(opts) != 2 || opts[0].SourceID != "1" || opts[1].Label != "Android" {
		t.Errorf("dropdown options by id: %+v", opts)
	}
}

// Boards from before API 2025-10 kept settings as a string with labels in a
// map by index, in the order labels_positions_v2 gives.
func TestFieldsOfBoardOlderSettings(t *testing.T) {
	status := `{"labels":{"0":"Working on it","1":"Done","2":"Stuck"},"labels_colors":{"0":{"color":"#fdab3d"},"1":{"color":"#00c875"},"2":{"color":"#e2445c"}},"labels_positions_v2":{"0":1,"1":0,"2":2}}`
	raw, _ := json.Marshal(status)
	b := mondayBoard{Columns: []mondayColumn{
		col("status", "status", "Status", ""),
		col("qa", "color", "QA", string(raw)),
		col("os", "dropdown", "OS", `{"labels":[{"id":4,"name":"Linux"}]}`),
		col("cost", "numeric", "Cost", `{"unit":{"symbol":"€"}}`),
	}}
	fields := fieldsOfBoard(b)
	qa := fields.byColumn["qa"]
	if qa.Type != importProvider.FieldSelect || len(qa.Options) != 3 || qa.Options[0].Label != "Done" || qa.Options[0].Color != "emerald" || qa.Options[1].Color != "amber" {
		t.Errorf("an older status column: %+v", qa)
	}
	if os := fields.byColumn["os"]; len(os.Options) != 1 || os.Options[0].Label != "Linux" || os.Options[0].SourceID != "4" {
		t.Errorf("an older dropdown: %+v", os)
	}
	if cost := fields.byColumn["cost"]; cost.Type != importProvider.FieldMoney || cost.Currency != "EUR" {
		t.Errorf("an older numbers column in euros: %+v", cost)
	}
}

func TestFieldValues(t *testing.T) {
	fields := fieldsOfBoard(roadmapBoard())
	item := mondayItem{ID: "11", ColumnValues: []mondayColumnValue{
		cv("status", "status", "Status", "Done", `{"index":1}`),
		cv("person", "people", "Owner", "Ada", `{"personsAndTeams":[{"id":4012,"kind":"person"}]}`),
		cv("approval", "status", "Approval", "Approved ✅", `{"index":0}`),
		cv("reviewer", "people", "Reviewer", "Ada, Grace", `{"personsAndTeams":[{"id":4012,"kind":"person"},{"id":4013,"kind":"person"}]}`),
		cv("platforms", "dropdown", "Platforms", "iOS, Android", `{"ids":[1,2]}`),
		cv("budget", "numbers", "Budget", "12500.5", `"12500.5"`),
		cv("hours", "numbers", "Hours", "", ""),
		cv("rating", "rating", "Score", "4", `{"rating":4}`),
		cv("launch", "date", "Launch", "2026-11-02", `{"date":"2026-11-02","changed_at":"2026-10-01"}`),
		cv("signed", "checkbox", "Signed off", "v", `{"checked":"true"}`),
		cv("brief", "link", "Brief", "Brief - https://docs.example.com/b", `{"url":"https://docs.example.com/b","text":"Brief"}`),
		cv("email", "email", "Contact", "ada@example.com", `{"email":"ada@example.com","text":"ada@example.com"}`),
		cv("notes", "long_text", "Notes", "Line one", `{"text":"Line one"}`),
		cv("formula", "formula", "Cost", "42", ""),
	}}
	values, carried := fieldValues(item, fields)
	got := map[string]any{}
	for _, v := range values {
		got[v.Field.Name] = v.Value
	}
	want := map[string]any{
		"Approval": "approved", "Reviewer": "4012", "Platforms": []string{"1", "2"}, "Budget": 12500.5,
		"Score": 4.0, "Launch": "2026-11-02", "Signed off": true, "Brief": "https://docs.example.com/b",
		"Contact": "ada@example.com", "Notes": "Line one",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values:\n got %v\nwant %v", got, want)
	}
	if carried["reviewer"] || !carried["approval"] || !carried["budget"] {
		t.Errorf("a field holding only the first of two people doesn't carry the column: %v", carried)
	}
	for _, v := range values {
		if (v.Field.Name == "Reviewer") != v.InDescription {
			t.Errorf("only the column the description keeps is marked as kept there: %+v", v)
		}
	}
	// The description keeps what no field carries whole.
	r := readItemFields(item, carried)
	var extra []string
	for _, kv := range r.Extra {
		extra = append(extra, kv[0])
	}
	if strings.Join(extra, ",") != "Reviewer,Cost" {
		t.Errorf("description keeps %v", extra)
	}
}

func TestFieldValueEdges(t *testing.T) {
	fields := fieldsOfBoard(roadmapBoard())
	values, _ := fieldValues(mondayItem{ColumnValues: []mondayColumnValue{
		// A label the column no longer offers comes along with its value.
		cv("approval", "status", "Approval", "On hold", `{"index":9}`),
		// Read by name when the value has no ids.
		cv("platforms", "dropdown", "Platforms", "Android", ""),
		cv("signed", "checkbox", "Signed off", "", `{"checked":true}`),
		cv("brief", "link", "Brief", "docs.example.com/c", ""),
		cv("budget", "numbers", "Budget", "lots", `"lots"`),
	}}, fields)
	byName := map[string]importProvider.SourceFieldValue{}
	for _, v := range values {
		byName[v.Field.Name] = v
	}
	hold := byName["Approval"]
	if hold.Value != "on hold" || hold.Field.Options[len(hold.Field.Options)-1].Label != "On hold" || len(fields.byColumn["approval"].Options) != 3 {
		t.Errorf("an unoffered label travels with its value, leaving the board's field as it was: %+v", hold)
	}
	if !reflect.DeepEqual(byName["Platforms"].Value, []string{"2"}) || byName["Signed off"].Value != true || byName["Brief"].Value != "docs.example.com/c" {
		t.Errorf("edges: %+v", byName)
	}
	if _, ok := byName["Budget"]; ok {
		t.Error("a number that isn't one has no value")
	}
}

// A dropdown label the column no longer offers still comes with the items
// that hold it, and every value keeps monday's own text of it.
func TestRetiredDropdownLabel(t *testing.T) {
	fields := fieldsOfBoard(roadmapBoard())
	values, carried := fieldValues(mondayItem{ColumnValues: []mondayColumnValue{
		cv("platforms", "dropdown", "Platforms", "iOS, Gone", `{"ids":[1,3]}`),
	}}, fields)
	if len(values) != 1 || !reflect.DeepEqual(values[0].Value, []string{"1", "3"}) || !carried["platforms"] {
		t.Fatalf("values: %+v", values)
	}
	opts := values[0].Field.Options
	if len(opts) != 3 || opts[2].Label != "Gone" || len(fields.byColumn["platforms"].Options) != 2 {
		t.Errorf("the retired label travels with the value only: %+v", opts)
	}
	if values[0].Text != "iOS, Gone" {
		t.Errorf("text: %q", values[0].Text)
	}
}
