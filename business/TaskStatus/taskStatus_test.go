package business

import (
	"errors"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestBuiltInNamesAreForgiving(t *testing.T) {
	for in, want := range map[string]string{
		"inProgress": "inProgress", "In Progress": "inProgress", "in-progress": "inProgress",
		"IN_REVIEW": "inReview", " done ": "done", "Canceled": "canceled", "todo": "todo",
	} {
		b := builtInByName(in)
		if b == nil || b.Key != want {
			t.Errorf("%q -> %v, want %s", in, b, want)
		}
	}
	if builtInByName("QA") != nil {
		t.Error("QA is not a built-in status")
	}
}

func TestValidate(t *testing.T) {
	ok := Input{Name: "  QA \n check ", Category: "inReview"}
	if err := ok.Validate(); err != nil || ok.Name != "QA check" || ok.Color != "slate" {
		t.Fatalf("got %+v, %v", ok, err)
	}
	for name, in := range map[string]Input{
		"empty name":         {Name: "   ", Category: "todo"},
		"too long":           {Name: strings.Repeat("a", MaxNameLength+1), Category: "todo"},
		"a built-in name":    {Name: "in progress", Category: "todo"},
		"unknown category":   {Name: "QA", Category: "shipping"},
		"custom as category": {Name: "QA", Category: ""},
		"unknown colour":     {Name: "QA", Category: "todo", Color: "chartreuse"},
	} {
		if err := in.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want invalid", name, err)
		}
	}
}

func TestFilterClause(t *testing.T) {
	id := "6f1f1ad2-4c55-4f3a-9f3e-0f9b1a0c2d3e"
	cases := map[string]struct {
		in   []string
		want string
	}{
		"built-in only shows tasks shown in it": {[]string{"done", "todo"}, `(anyofterms(task_status, "done todo") AND NOT has(task_custom_status))`},
		"custom only":                           {[]string{id}, `eq(task_custom_status, ["` + id + `"])`},
		"both":                                  {[]string{"done", id}, `((anyofterms(task_status, "done") AND NOT has(task_custom_status)) OR eq(task_custom_status, ["` + id + `"]))`},
		"nothing usable":                        {[]string{"nope", `x") OR has(user_uuid`}, ""},
	}
	for name, c := range cases {
		if got := FilterClause(c.in); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, c.want)
		}
	}
}

func TestOfAndDisplay(t *testing.T) {
	id, name := "abc", "QA"
	r := Of(&dgraphStruct.DgraphTask{Status: "inReview", CustomStatus: &id, CustomStatusName: &name})
	if r.Category != "inReview" || r.CustomID != "abc" || r.Display() != "QA" {
		t.Fatalf("custom: %+v %q", r, r.Display())
	}
	empty := ""
	r = Of(&dgraphStruct.DgraphTask{Status: "inProgress", CustomStatus: &empty})
	if r.CustomID != "" || r.Display() != "In Progress" {
		t.Fatalf("built-in: %+v %q", r, r.Display())
	}
}

func TestResolvedValue(t *testing.T) {
	if v := (Resolved{Category: "inReview"}).Value(); v != "inReview" {
		t.Fatal(v)
	}
	if v := (Resolved{Category: "inReview", CustomID: "id-1", CustomName: "QA"}).Value(); v != "id-1" {
		t.Fatal(v)
	}
}

func TestRepointMatchesAsResolveReads(t *testing.T) {
	r := Repoint{Old: []string{"id-1", "Quality check", ""}}
	for v, want := range map[string]bool{"id-1": true, " quality CHECK ": true, "QA": false, "": false} {
		if r.Matches(v) != want {
			t.Errorf("%q: want %v", v, want)
		}
	}
}
