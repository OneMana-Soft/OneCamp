package trello

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A board export (or GET /boards/{id}?customFields=true&card_customFieldItems=true)
// with the Custom Fields power-up on.
const boardWithFields = `{"id":"b1","name":"Launch","customFields":[
  {"id":"f1","name":"Stage","type":"list","options":[{"id":"o1","color":"green","value":{"text":"Draft"}},{"id":"o2","color":"sky","value":{"text":"Live"}}]},
  {"id":"f2","name":"Signed off","type":"checkbox"},
  {"id":"f3","name":"Launch","type":"date"},
  {"id":"f4","name":"Budget","type":"number"},
  {"id":"f5","name":"Client","type":"text"},
  {"id":"f6","name":"Mystery","type":"rating"}
],
"cards":[{"id":"c1","name":"Post","customFieldItems":[
  {"idCustomField":"f1","idValue":"o2"},
  {"idCustomField":"f2","value":{"checked":"true"}},
  {"idCustomField":"f3","value":{"date":"2026-11-02T12:00:00.000Z"}},
  {"idCustomField":"f4","value":{"number":"12.5"}},
  {"idCustomField":"f5","value":{"text":"Acme"}},
  {"idCustomField":"f6","value":{"number":"3"}},
  {"idCustomField":"gone","value":{"text":"x"}}
]},{"id":"c2","name":"Unticked","customFieldItems":[{"idCustomField":"f2","value":{"checked":"false"}}]}]}`

func TestBoardFields(t *testing.T) {
	var b trelloBoard
	if err := json.Unmarshal([]byte(boardWithFields), &b); err != nil {
		t.Fatal(err)
	}
	list, byID := boardFields(b.CustomFields)
	var kinds []string
	for _, f := range list {
		kinds = append(kinds, f.Name+":"+f.Type)
	}
	if strings.Join(kinds, " ") != "Stage:select Signed off:checkbox Launch:date Budget:number Client:text" {
		t.Fatalf("fields: %v", kinds)
	}
	if opts := byID["f1"].Options; len(opts) != 2 || opts[1].Label != "Live" || opts[1].Color != "sky" {
		t.Errorf("dropdown options: %+v", opts)
	}
	got := map[string]any{}
	for _, v := range cardFieldValues(b.Cards[0].CustomFieldItems, byID) {
		got[v.Field.Name] = v.Value
	}
	want := map[string]any{"Stage": "o2", "Signed off": true, "Launch": "2026-11-02", "Budget": 12.5, "Client": "Acme"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values:\n got %v\nwant %v", got, want)
	}
	if vals := cardFieldValues(b.Cards[1].CustomFieldItems, byID); len(vals) != 0 {
		t.Errorf("an unticked box has no value: %+v", vals)
	}
}
