package asana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

func TestFieldOf(t *testing.T) {
	no := false
	cases := []struct {
		def  asanaFieldDef
		want string // name:type+currency, or "" when it isn't brought across
	}{
		{asanaFieldDef{GID: "1", Name: "Notes", ResourceSubtype: "text"}, "Notes:text"},
		{asanaFieldDef{GID: "2", Name: "Budget", ResourceSubtype: "number", Format: "currency", CurrencyCode: "EUR"}, "Budget:moneyEUR"},
		{asanaFieldDef{GID: "3", Name: "Done %", ResourceSubtype: "number", Format: "percentage"}, "Done %:number"},
		{asanaFieldDef{GID: "4", Name: "Stage", Type: "enum", EnumOptions: []asanaEnumOption{
			{GID: "o1", Name: "Draft", Color: "yellow-orange"}, {GID: "o2", Name: "Retired", Enabled: &no}, {GID: "o3", Name: "Live", Color: "blue-green"},
		}}, "Stage:select"},
		{asanaFieldDef{GID: "5", Name: "Channels", ResourceSubtype: "multi_enum"}, "Channels:multi_select"},
		{asanaFieldDef{GID: "6", Name: "Launch", ResourceSubtype: "date"}, "Launch:date"},
		{asanaFieldDef{GID: "7", Name: "Reviewer", ResourceSubtype: "people"}, "Reviewer:person"},
		{asanaFieldDef{GID: "8", Name: "Cost", ResourceSubtype: "formula"}, ""},
		{asanaFieldDef{GID: "11", Name: "Margin", ResourceSubtype: "number", IsFormula: true}, ""},
		// Any currency code Asana names, not only common ones.
		{asanaFieldDef{GID: "12", Name: "Fee", ResourceSubtype: "number", Format: "currency", CurrencyCode: "MUR"}, "Fee:moneyMUR"},
		{asanaFieldDef{GID: "9", Name: "ID", ResourceSubtype: "custom_id"}, ""},
		// Priority is the task's priority already.
		{asanaFieldDef{GID: "10", Name: " priority ", ResourceSubtype: "enum"}, ""},
	}
	for _, c := range cases {
		f, ok := fieldOf(c.def)
		got := ""
		if ok {
			got = f.Name + ":" + f.Type + f.Currency
		}
		if got != c.want {
			t.Errorf("%+v: %q, want %q", c.def, got, c.want)
		}
	}
	stage, _ := fieldOf(cases[3].def)
	if v := fieldValues([]asanaCustomField{{GID: "1", Name: "Notes", ResourceSubtype: "text", TextValue: ptr("Hi"), DisplayValue: "Hi"}}); len(v) != 1 || v[0].Text != "Hi" {
		t.Errorf("a value keeps Asana's text of it: %+v", v)
	}
	// A formula's value has no field to go in: it comes as text, typeless, for
	// the task's description.
	if v := fieldValues([]asanaCustomField{{GID: "9", Name: "Margin", ResourceSubtype: "number", IsFormula: true, NumberValue: ptr(0.4), DisplayValue: "40%"}}); len(v) != 1 || v[0].Field.Type != "" || v[0].Text != "40%" {
		t.Errorf("a formula's value: %+v", v)
	}
	if !reflect.DeepEqual(stage.Options, []importProvider.SourceOption{{SourceID: "o1", Label: "Draft", Color: "amber"}, {SourceID: "o3", Label: "Live", Color: "teal"}}) {
		t.Errorf("enabled options, colours matched: %+v", stage.Options)
	}
}

// A task as GET /projects/{gid}/tasks returns it with our opt_fields.
const taskJSON = `{"gid":"t1","name":"Launch post","custom_fields":[
  {"gid":"1","name":"Notes","resource_subtype":"text","text_value":"Short and warm","display_value":"Short and warm"},
  {"gid":"2","name":"Budget","resource_subtype":"number","format":"currency","currency_code":"EUR","number_value":1250.5},
  {"gid":"4","name":"Stage","resource_subtype":"enum","enum_value":{"gid":"o3","name":"Live","color":"blue-green"}},
  {"gid":"5","name":"Channels","resource_subtype":"multi_enum","multi_enum_values":[{"gid":"c1","name":"Blog"},{"gid":"c2","name":"Email","color":"aqua"}]},
  {"gid":"6","name":"Launch","resource_subtype":"date","date_value":{"date":"2026-11-02","date_time":null}},
  {"gid":"7","name":"Reviewer","resource_subtype":"people","people_value":[{"gid":"u9","name":"Maya"}]},
  {"gid":"10","name":"Priority","resource_subtype":"enum","enum_value":{"gid":"p1","name":"High"},"display_value":"High"},
  {"gid":"11","name":"Empty","resource_subtype":"text","text_value":null},
  {"gid":"12","name":"No stage","resource_subtype":"enum","enum_value":null},
  {"gid":"8","name":"Cost","resource_subtype":"formula","number_value":99}
]}`

func TestTaskFieldValues(t *testing.T) {
	var task asanaTask
	if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
		t.Fatal(err)
	}
	st := buildSourceTask(&task, "p1", nil, "")
	if st.Priority != "High" {
		t.Errorf("Priority stays the task's priority: %q", st.Priority)
	}
	got := map[string]any{}
	for _, v := range st.Fields {
		got[v.Field.Name] = v.Value
	}
	want := map[string]any{
		"Notes": "Short and warm", "Budget": 1250.5, "Stage": "o3", "Channels": []string{"c1", "c2"},
		"Launch": "2026-11-02", "Reviewer": "u9",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values:\n got %v\nwant %v", got, want)
	}
	for _, v := range st.Fields {
		if v.Field.Name == "Channels" && (len(v.Field.Options) != 2 || v.Field.Options[1].Color != "cyan") {
			t.Errorf("a value names its options, so the field can be made from it: %+v", v.Field)
		}
	}
}

func TestListProjectFields(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("offset") == "" {
			fmt.Fprint(w, `{"data":[{"custom_field":{"gid":"4","name":"Stage","resource_subtype":"enum","enum_options":[{"gid":"o1","name":"Draft","enabled":true}]}},
			  {"custom_field":{"gid":"8","name":"Cost","resource_subtype":"formula"}}],
			  "next_page":{"offset":"x","path":"/api/1.0/projects/p1/custom_field_settings?limit=100&offset=x"}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"custom_field":{"gid":"2","name":"Budget","resource_subtype":"number","format":"currency","currency_code":"USD"}}],"next_page":null}`)
	}))
	defer srv.Close()
	defer func(old string) { apiBase = old }(apiBase)
	apiBase = srv.URL + "/api/1.0"

	fields, err := New().listProjectFields(context.Background(), "tok", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields[0].Name != "Stage" || len(fields[0].Options) != 1 || fields[1].Currency != "USD" {
		t.Fatalf("both pages, formulas left out: %+v", fields)
	}
	if len(asked) != 2 || !strings.Contains(asked[0], "custom_field.enum_options.name") {
		t.Errorf("requests: %v", asked)
	}
}

func ptr[T any](v T) *T { return &v }
