package business

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// opts reads a JSON list of options as a field config's.
func opts(t *testing.T, list string) []map[string]interface{} {
	t.Helper()
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(`{"options":`+list+`}`), &cfg); err != nil {
		t.Fatal(err)
	}
	return optionsIn(cfg)
}

// save plans saving a field with options sent over one with stored, the ids
// in removed taken out, and gives back the ids the options were given and how
// the cells change.
func save(t *testing.T, stored, sent string, removed ...string) ([]string, map[string]string) {
	t.Helper()
	return plan(t, false, stored, sent, removed...)
}

// plan is save, for a save of a whole field when whole.
func plan(t *testing.T, whole bool, stored, sent string, removed ...string) ([]string, map[string]string) {
	t.Helper()
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(`{"options":`+sent+`}`), &cfg); err != nil {
		t.Fatal(err)
	}
	var old []map[string]interface{}
	if stored != "" {
		old = opts(t, stored)
	}
	out := map[string]bool{}
	for _, id := range removed {
		out[id] = true
	}
	changes := planOptions(old, cfg, out, whole)
	var ids []string
	for _, o := range optionsIn(cfg) {
		ids = append(ids, optionID(o))
	}
	return ids, changes
}

func TestSelectOptionsKeepTheirIDs(t *testing.T) {
	legacy := `[{"label":"Todo","color":"red"},{"label":"Doing"},{"label":"Done"}]`
	withIDs := `[{"id":"a","label":"Todo"},{"id":"b","label":"Doing"},{"id":"c","label":"Done"}]`
	cases := []struct {
		name         string
		stored, sent string
		removed      []string
		ids          []string
		changes      map[string]string
		newIDsAt     []int // places whose id is a new one (o_…)
	}{
		{name: "a new field's options are known by their labels",
			sent: `[{"label":"Todo"},{"label":"Done"}]`, ids: []string{"label:Todo", "label:Done"}},
		{name: "the web renames an option stored without an id, by the id it worked out",
			stored: legacy, sent: `[{"id":"label:Todo","label":"To do"},{"id":"label:Doing","label":"Doing"},{"id":"label:Done","label":"Done"}]`,
			ids: []string{"label:Todo", "label:Doing", "label:Done"}, changes: map[string]string{"Todo": "To do"}},
		{name: "a new label in a list that needn't name every option is a new option",
			stored: legacy, sent: `[{"label":"Todo"},{"label":"In progress"},{"label":"Done"}]`,
			ids: []string{"label:Todo", "label:In progress", "label:Done", "label:Doing"}, changes: map[string]string{}},
		// A save that left an option out, which it keeps, then one adding an
		// option at the end: in place of the one left out, the new option
		// renamed it, in every cell holding it.
		{name: "an option added after one left out takes nobody's place",
			stored: withIDs, sent: `[{"label":"Todo"},{"label":"Doing"},{"label":"Paused"}]`,
			ids: []string{"a", "b", "label:Paused", "c"}, changes: map[string]string{}},
		{name: "options moved without ids are matched by label, and nothing changes",
			stored: legacy, sent: `[{"label":"Done"},{"label":"Todo"},{"label":"Doing"}]`,
			ids: []string{"label:Done", "label:Todo", "label:Doing"}, changes: map[string]string{}},
		{name: "options moved by id change nothing",
			stored: withIDs, sent: `[{"id":"c","label":"Done"},{"id":"a","label":"Todo"},{"id":"b","label":"Doing"}]`,
			ids: []string{"c", "a", "b"}, changes: map[string]string{}},
		{name: "a caller sending labels alone keeps the ids",
			stored: withIDs, sent: `[{"label":"Todo"},{"label":"Doing"},{"label":"Done"}]`,
			ids: []string{"a", "b", "c"}, changes: map[string]string{}},
		{name: "two options swapping names swap in their cells",
			stored: withIDs, sent: `[{"id":"a","label":"Doing"},{"id":"b","label":"Todo"},{"id":"c","label":"Done"}]`,
			ids: []string{"a", "b", "c"}, changes: map[string]string{"Todo": "Doing", "Doing": "Todo"}},
		{name: "a removed option's cells are cleared",
			stored: withIDs, sent: `[{"id":"a","label":"Todo"},{"id":"c","label":"Done"}]`, removed: []string{"b"},
			ids: []string{"a", "c"}, changes: map[string]string{"Doing": ""}},
		{name: "an option a save leaves out is kept, and so are its cells",
			stored: withIDs, sent: `[{"id":"a","label":"Todo"},{"id":"c","label":"Done"}]`,
			ids: []string{"a", "c", "b"}, changes: map[string]string{}},
		{name: "a save from before the options were added keeps them all",
			stored: withIDs, sent: `[{"id":"o_new","label":"Blocked"}]`,
			ids: []string{"o_new", "a", "b", "c"}, changes: map[string]string{}},
		{name: "a save with no options keeps them all",
			stored: withIDs, sent: `null`,
			ids: []string{"a", "b", "c"}, changes: map[string]string{}},
		{name: "a removed option whose label another has keeps its cells",
			stored: withIDs, sent: `[{"id":"a","label":"Todo"},{"id":"x","label":"Doing"},{"id":"c","label":"Done"}]`, removed: []string{"b"},
			ids: []string{"a", "x", "c"}, changes: map[string]string{}},
		{name: "an option stored without an id is removed by the id worked out for it",
			stored: legacy, sent: `[{"id":"label:Todo","label":"Todo"},{"id":"label:Done","label":"Done"}]`, removed: []string{"label:Doing"},
			ids: []string{"label:Todo", "label:Done"}, changes: map[string]string{"Doing": ""}},
		{name: "a new option at the end takes nobody's place",
			stored: withIDs, sent: `[{"label":"Todo"},{"label":"Doing"},{"label":"Done"},{"label":"Blocked"}]`,
			ids: []string{"a", "b", "c", "label:Blocked"}, changes: map[string]string{}},
		{name: "an id sent twice is given once",
			stored: withIDs, sent: `[{"id":"a","label":"Todo"},{"id":"a","label":"Again"}]`,
			ids: []string{"a", "label:Again", "b", "c"}, changes: map[string]string{}},
		{name: "a new option whose label's id is stored gets a new id",
			stored: `[{"id":"label:Todo","label":"To do"}]`, sent: `[{"id":"label:Todo","label":"To do"},{"label":"Todo"}]`,
			ids: []string{"label:Todo", ""}, changes: map[string]string{}, newIDsAt: []int{1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids, changes := save(t, c.stored, c.sent, c.removed...)
			for _, i := range c.newIDsAt {
				if !strings.HasPrefix(ids[i], "o_") || len(ids[i]) != 12 {
					t.Errorf("option %d's id is %q, want a new o_ id", i, ids[i])
				}
				ids[i] = ""
			}
			if !reflect.DeepEqual(ids, c.ids) {
				t.Errorf("ids %v, want %v", ids, c.ids)
			}
			if c.changes == nil {
				c.changes = map[string]string{}
			}
			if !reflect.DeepEqual(changes, c.changes) {
				t.Errorf("cell changes %v, want %v", changes, c.changes)
			}
		})
	}
}

// A save of a whole field, as the web released before ids (v2.72.2's) sends
// every one, is read as v2.72.2 read it: the options it lists are the
// field's. One it leaves out is taken out of the field, and its cells keep
// their label, as they did then. That web sends back the ids it read, so a
// rename is found by its id once a save has stored them.
func TestAWholeListIsTheFieldsOptions(t *testing.T) {
	legacy := `[{"label":"Todo","color":"red"},{"label":"Doing"},{"label":"Done"}]`
	withIDs := `[{"id":"label:Todo","label":"Todo","color":"red"},{"id":"label:Doing","label":"Doing"},{"id":"label:Done","label":"Done"}]`
	cases := []struct {
		name         string
		stored, sent string
		ids          []string
		changes      map[string]string
	}{
		{name: "an option left out is taken out, and its cells keep their label",
			stored: legacy, sent: `[{"label":"Todo","color":"red"},{"label":"Done"}]`,
			ids: []string{"label:Todo", "label:Done"}, changes: map[string]string{}},
		{name: "so it is once the options have ids",
			stored: withIDs, sent: `[{"id":"label:Todo","label":"Todo","color":"red"},{"id":"label:Done","label":"Done"}]`,
			ids: []string{"label:Todo", "label:Done"}, changes: map[string]string{}},
		{name: "a rename of a field saved before ids leaves its cells as they are, as v2.72.2 did",
			stored: legacy, sent: `[{"label":"Todo","color":"red"},{"label":"In progress"},{"label":"Done"}]`,
			ids: []string{"label:Todo", "label:In progress", "label:Done"}, changes: map[string]string{}},
		// Another tab added Blocked; this one, read before, adds Cancelled
		// where Blocked now is: Blocked's cells were given Cancelled's label.
		{name: "a list read before an option was added takes it out, and gives its cells no other label",
			stored: `[{"id":"label:Todo","label":"Todo"},{"id":"label:Doing","label":"Doing"},{"id":"label:Done","label":"Done"},{"id":"o_blocked001","label":"Blocked"}]`,
			sent:   `[{"id":"label:Todo","label":"Todo"},{"id":"label:Doing","label":"Doing"},{"id":"label:Done","label":"Done"},{"label":"Cancelled"}]`,
			ids:    []string{"label:Todo", "label:Doing", "label:Done", "label:Cancelled"}, changes: map[string]string{}},
		{name: "so is one renamed by its id",
			stored: withIDs, sent: `[{"id":"label:Todo","label":"To do","color":"red"},{"id":"label:Doing","label":"Doing"},{"id":"label:Done","label":"Done"}]`,
			ids: []string{"label:Todo", "label:Doing", "label:Done"}, changes: map[string]string{"Todo": "To do"}},
		{name: "an option added at the end takes nobody's place",
			stored: withIDs, sent: `[{"id":"label:Todo","label":"Todo"},{"id":"label:Doing","label":"Doing"},{"id":"label:Done","label":"Done"},{"label":"Blocked"}]`,
			ids: []string{"label:Todo", "label:Doing", "label:Done", "label:Blocked"}, changes: map[string]string{}},
		{name: "options moved change nothing",
			stored: legacy, sent: `[{"label":"Done"},{"label":"Todo","color":"red"},{"label":"Doing"}]`,
			ids: []string{"label:Done", "label:Todo", "label:Doing"}, changes: map[string]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids, changes := plan(t, true, c.stored, c.sent)
			if !reflect.DeepEqual(ids, c.ids) {
				t.Errorf("ids %v, want %v", ids, c.ids)
			}
			if !reflect.DeepEqual(changes, c.changes) {
				t.Errorf("cell changes %v, want %v", changes, c.changes)
			}
		})
	}
}

func TestSelectOptionsKeepWhatElseTheyCarry(t *testing.T) {
	cfg := map[string]interface{}{"options": []interface{}{map[string]interface{}{"label": "Todo", "color": "red"}}, "tint": true}
	planOptions(nil, cfg, nil, false)
	b, _ := json.Marshal(cfg)
	if string(b) != `{"options":[{"color":"red","id":"label:Todo","label":"Todo"}],"tint":true}` {
		t.Errorf("the config saved is %s", b)
	}
}

// The AI draft builds its options as a Go list of maps; they keep their
// labels and get ids.
func TestSelectOptionsFromAGoList(t *testing.T) {
	cfg := map[string]interface{}{"options": []map[string]interface{}{{"label": "High"}, {"label": "Low"}}}
	planOptions(nil, cfg, nil, false)
	b, _ := json.Marshal(cfg)
	if string(b) != `{"options":[{"id":"label:High","label":"High"},{"id":"label:Low","label":"Low"}]}` {
		t.Errorf("the config saved is %s", b)
	}
}
