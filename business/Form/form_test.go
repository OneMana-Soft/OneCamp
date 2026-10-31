package business

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/business/Formula"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
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
		{Title: "x", Fields: []Field{{Label: "a", Type: "signature"}}},
		{Title: "x", Fields: []Field{{Label: "a", Type: Select, Options: []string{"only"}}}},
		{Title: "x", Fields: []Field{{Id: "a", Label: "a", Type: ShortText}, {Id: "a", Label: "b", Type: ShortText}}},
		{Title: "x", Fields: []Field{{Label: "a", Type: ShortText}}, Priority: "someday"},
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
	}, formula.NumberSigns{})
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
		// Read as tables read numbers: neither of these is one.
		{"what": "x", "email": "a@b.co", "n": "NaN"},
		{"what": "x", "email": "a@b.co", "n": "Infinity"},
		{"what": strings.Repeat("x", 301), "email": "a@b.co"},
		{"what": []any{"x"}, "email": "a@b.co"},
	}
	for i, raw := range bad {
		if _, err := CheckAnswers(fields(), raw, formula.NumberSigns{}); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// A number answer is read as tables read numbers people write: "1,200" and
// "€40" are numbers, which ParseFloat refused, and NaN isn't.
func TestNumberAnswersAsPeopleWriteThem(t *testing.T) {
	for _, n := range []string{"1,200", "€40", " 12.5 "} {
		if _, err := CheckAnswers(fields(), map[string]any{"what": "x", "email": "a@b.co", "n": n}, formula.NumberSigns{}); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
}

// A number answer is read in the signs of whoever sent it, which the form
// sends with the answers: "1,5" from Berlin is one and a half. Read in
// en-US's signs it is refused, since a comma there groups digits, and never
// read as fifteen.
func TestNumberAnswersInTheSendersSigns(t *testing.T) {
	de := formula.NumberSigns{Decimal: ",", Group: "."}
	for n, want := range map[string]string{"1,5": "1,5", "1.234,5": "1.234,5", "€ 12,50": "€ 12,50"} {
		a, err := CheckAnswers(fields(), map[string]any{"what": "x", "email": "a@b.co", "n": n}, de)
		if err != nil {
			t.Errorf("%q in de-DE's signs: %v", n, err)
			continue
		}
		for _, x := range a {
			if x.Label == "Seats" && x.Value != want {
				t.Errorf("%q kept as %q", n, x.Value)
			}
		}
	}
	if _, err := CheckAnswers(fields(), map[string]any{"what": "x", "email": "a@b.co", "n": "1,5"}, formula.NumberSigns{}); err == nil {
		t.Error(`"1,5" in en-US's signs was read as a number`)
	}
	// Signs no number is written with are refused, not guessed at.
	if _, err := CheckAnswers(fields(), map[string]any{"what": "x", "email": "a@b.co", "n": "1"}, formula.NumberSigns{Decimal: "x", Group: "."}); err == nil {
		t.Error("a letter for a decimal sign was taken")
	}
}

func TestTaskFrom(t *testing.T) {
	name, desc := TaskFrom("Requests", fields(), "what", []Answer{
		{Id: "what", Label: "What do you need?", Value: "<script>alert(1)</script> logo"},
		{Id: "more", Label: "Details", Value: "line one\nline two"},
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

// A sent form tells the rules its project's name as well as its own, so a
// form rule's words can say {project}, as a task rule's do.
func TestASentFormNamesItsProject(t *testing.T) {
	form, project, sent, task := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	l := &live{
		form:    &formModel.Form{Id: form, Title: "Requests", ProjectUUID: project},
		owner:   &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: "owner-uuid"}},
		project: &dgraphStruct.DgraphProject{Uuid: project.String(), Name: "Q4 launch"},
	}
	ev := submitted(l, sent, task, "New logo", []Answer{{Id: "team", Label: "Team", Value: "Design"}})
	want := map[string]interface{}{
		"event_id": sent.String(), "form_id": form.String(), "form_title": "Requests", "task_id": task.String(),
		"project_id": project.String(), "project_name": "Q4 launch", "name": "New logo", "created_by": "owner-uuid",
	}
	for k, v := range want {
		if ev[k] != v {
			t.Errorf("%s is %v, want %v", k, ev[k], v)
		}
	}
	if a, _ := ev["answers"].(map[string]interface{}); a["team"] != "Design" {
		t.Errorf("answers %v", ev["answers"])
	}
}

// Tables B5b (airtable-07): a hidden question is filled only by its link, so
// it is never required and is never a section or a file; a form may send its
// visitor to an http(s) page once it's sent, and to nothing else.
func TestAFormHidesQuestionsAndSendsItsVisitorOn(t *testing.T) {
	base := func(fields []Field, redirect string) Input {
		return Input{Title: "Sign-up", Active: true, Fields: fields, Redirect: redirect}
	}
	q := func(over Field) Field {
		f := Field{Id: "src", Label: "Source", Type: ShortText}
		if over.Type != "" {
			f.Type = over.Type
		}
		f.Hidden, f.Required = over.Hidden, over.Required
		return f
	}
	name := Field{Id: "name", Label: "Name", Type: ShortText, Required: true}
	f, err := CheckForm(base([]Field{name, q(Field{Hidden: true})}, " https://example.com/thanks?from=form "))
	if err != nil {
		t.Fatal(err)
	}
	var kept []Field
	_ = json.Unmarshal(f.Fields, &kept)
	if !kept[1].Hidden || f.RedirectURL != "https://example.com/thanks?from=form" {
		t.Errorf("kept hidden %v, redirect %q", kept[1].Hidden, f.RedirectURL)
	}
	var fe *FormError
	for what, in := range map[string]Input{
		"a hidden required question": base([]Field{name, q(Field{Hidden: true, Required: true})}, ""),
		"a hidden section":           base([]Field{name, {Id: "s", Label: "More", Type: Section, Hidden: true}}, ""),
		"a hidden file question":     base([]Field{name, q(Field{Hidden: true, Type: File})}, ""),
		"a redirect to javascript:":  base([]Field{name}, "javascript:alert(1)"),
		"a redirect with no host":    base([]Field{name}, "https:///thanks"),
		"a redirect with a password": base([]Field{name}, "https://me:pw@example.com/"),
		"a redirect that's a path":   base([]Field{name}, "/thanks"),
		"a redirect too long":        base([]Field{name}, "https://example.com/"+strings.Repeat("a", 500)),
	} {
		if _, err := CheckForm(in); !errors.As(err, &fe) {
			t.Errorf("%s: %v, want a FormError", what, err)
		}
	}
}
