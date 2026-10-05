package business

import (
	"encoding/json"
	"strings"
	"testing"
)

func fields() []Field {
	return []Field{
		{Id: "what", Label: "What do you need?", Type: ShortText, Required: true},
		{Id: "email", Label: "Your email", Type: Email, Required: true},
		{Id: "team", Label: "Team", Type: Select, Options: []string{"Sales", "Design"}},
		{Id: "when", Label: "Needed by", Type: Date},
		{Id: "n", Label: "Seats", Type: Number},
		{Id: "urgent", Label: "Urgent", Type: Checkbox},
		{Id: "more", Label: "Details", Type: LongText},
	}
}

func TestCheckForm(t *testing.T) {
	f, err := CheckForm(Input{Title: " Design  requests ", Fields: []Field{
		{Label: "What", Type: ShortText}, {Label: "Team", Type: Select, Options: []string{" A ", "", "B"}},
	}})
	if err != nil || f.Title != "Design requests" || f.TitleField != "q1" || f.Priority != "medium" {
		t.Fatalf("got %+v %v", f, err)
	}
	var stored []Field
	_ = json.Unmarshal(f.Fields, &stored)
	if stored[1].Id != "q2" || len(stored[1].Options) != 2 || stored[1].Options[0] != "A" {
		t.Fatalf("fields %+v", stored)
	}
	bad := []Input{
		{Title: "", Fields: []Field{{Label: "a", Type: ShortText}}},
		{Title: "x"},
		{Title: "x", Fields: []Field{{Label: "", Type: ShortText}}},
		{Title: "x", Fields: []Field{{Label: "a", Type: "file"}}},
		{Title: "x", Fields: []Field{{Label: "a", Type: Select, Options: []string{"only"}}}},
		{Title: "x", Fields: []Field{{Id: "a", Label: "a", Type: ShortText}, {Id: "a", Label: "b", Type: ShortText}}},
		{Title: "x", Fields: []Field{{Label: "a", Type: ShortText}}, Priority: "urgent"},
	}
	for i, in := range bad {
		if _, err := CheckForm(in); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestCheckAnswers(t *testing.T) {
	a, err := CheckAnswers(fields(), map[string]any{
		"what": " New logo ", "email": "Sam <sam@example.com>", "team": "Design", "when": "2026-10-20", "n": float64(3), "urgent": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, x := range a {
		got[x.Label] = x.Value
	}
	if got["What do you need?"] != "New logo" || got["Your email"] != "sam@example.com" || got["Seats"] != "3" || got["Urgent"] != "Yes" {
		t.Fatalf("got %v", got)
	}
	if _, ok := got["Details"]; ok {
		t.Fatal("an empty optional answer was kept")
	}
	bad := []map[string]any{
		{"email": "a@b.co"},
		{"what": "x", "email": "nope"},
		{"what": "x", "email": "a@b.co", "team": "Ops"},
		{"what": "x", "email": "a@b.co", "when": "20/10/2026"},
		{"what": "x", "email": "a@b.co", "n": "many"},
		{"what": strings.Repeat("x", 301), "email": "a@b.co"},
		{"what": []any{"x"}, "email": "a@b.co"},
	}
	for i, raw := range bad {
		if _, err := CheckAnswers(fields(), raw); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestTaskFrom(t *testing.T) {
	name, desc := TaskFrom("Requests", fields(), "what", []Answer{
		{Label: "What do you need?", Value: "<script>alert(1)</script> logo"},
		{Label: "Details", Value: "line one\nline two"},
	})
	if name != "<script>alert(1)</script> logo" {
		t.Fatalf("name %q", name)
	}
	if strings.Contains(desc, "<script>") || !strings.Contains(desc, "&lt;script&gt;") || !strings.Contains(desc, "line one<br>line two") {
		t.Fatalf("description not escaped: %s", desc)
	}
	if n, _ := TaskFrom("Requests", fields(), "what", nil); n != "Requests response" {
		t.Fatalf("fallback name %q", n)
	}
}
