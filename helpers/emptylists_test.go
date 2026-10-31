package helpers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type emptyListView struct {
	Name  string   `json:"name"`
	Tasks []string `json:"tasks"`
}

// V-52: what an answer's envelope says for each kind of "none".
func TestEmptyListsInResponseAnswersANilListAsEmpty(t *testing.T) {
	var nilViews []emptyListView
	var nilByProject map[string][]emptyListView
	var nilPointer *emptyListView
	cases := []struct {
		name string
		data interface{}
		want string
	}{
		{"a nil slice in an envelope", Envolope{"data": nilViews}, `{"data":[]}`},
		{"a nil slice in a nested envelope", Envolope{"data": Envolope{"milestones": nilViews, "can_manage": true}}, `{"data":{"can_manage":true,"milestones":[]}}`},
		{"a plain map envelope, nested", map[string]interface{}{"data": map[string]interface{}{"docs": []string(nil), "boards": []int(nil)}}, `{"data":{"boards":[],"docs":[]}}`},
		{"a nil map", Envolope{"by_project": nilByProject}, `{"by_project":{}}`},
		{"a nil []interface{}", Envolope{"items": []interface{}(nil)}, `{"items":[]}`},
		{"a list with items, as it was", Envolope{"data": []string{"a"}}, `{"data":["a"]}`},
		{"an empty list, as it was", Envolope{"data": []string{}}, `{"data":[]}`},
		{"an untyped nil: none of one thing", Envolope{"data": nil}, `{"data":null}`},
		{"a nil pointer", Envolope{"data": nilPointer}, `{"data":null}`},
		{"nil bytes", Envolope{"raw": []byte(nil)}, `{"raw":null}`},
		{"a nil RawMessage", Envolope{"raw": json.RawMessage(nil)}, `{"raw":null}`},
		{"a nil envelope inside", Envolope{"data": Envolope(nil)}, `{"data":null}`},
		// A struct marshals its own fields: the pass doesn't reach them (fixed per surface).
		{"a struct's own nil field", Envolope{"data": emptyListView{Name: "Beta"}}, `{"data":{"name":"Beta","tasks":null}}`},
		{"a struct answer whole", emptyListView{Name: "Beta"}, `{"name":"Beta","tasks":null}`},
		{"a list answer whole", []string(nil), `null`},
	}
	for _, c := range cases {
		got, err := json.Marshal(EmptyListsInResponse(c.data))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestEmptyListsInResponseLeavesItsInputAsItWas(t *testing.T) {
	inner := Envolope{"tasks": []string(nil)}
	outer := Envolope{"data": inner, "msg": "ok"}
	_ = EmptyListsInResponse(outer)
	if outer["data"].(Envolope)["tasks"].([]string) != nil {
		t.Fatal("the caller's envelope was changed")
	}
	// Nothing to change: the same map back, not a copy.
	same := Envolope{"data": []string{"a"}}
	if got := EmptyListsInResponse(same).(Envolope); reflect.ValueOf(got).Pointer() != reflect.ValueOf(same).Pointer() {
		t.Fatal("an envelope with nothing to change was copied")
	}
}

// Through the one writer every handler answers with.
func TestWriteJSONAnswersANilListAsEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, Envolope{"msg": "ok", "data": Envolope{"views": []emptyListView(nil)}})
	var got struct {
		Data struct {
			Views json.RawMessage `json:"views"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rec.Body.String())
	}
	if string(got.Data.Views) != "[]" {
		t.Fatalf("views went as %s, want []", got.Data.Views)
	}
}

func TestNonNil(t *testing.T) {
	if got := NonNil([]string(nil)); got == nil || len(got) != 0 {
		t.Fatalf("NonNil(nil) = %#v", got)
	}
	in := []int{1, 2}
	if got := NonNil(in); &got[0] != &in[0] {
		t.Fatal("NonNil copied a list that wasn't nil")
	}
}
