package business

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Formula"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// A request form that branches: a bug asks for steps, an idea opens a page of
// its own, and a link is asked only of the web.
func branching() []Field {
	return []Field{
		{Id: "what", Label: "What do you need?", Type: ShortText, Required: true},
		{Id: "kind", Label: "Kind", Type: Select, Required: true, Options: []string{"Bug", "Idea"}},
		{Id: "steps", Label: "Steps to reproduce", Type: LongText, Required: true, ShowIf: &ShowIf{Question: "kind", Op: ShowIs, Values: []string{"Bug"}}},
		{Id: "s2", Label: "The idea", Type: Section, ShowIf: &ShowIf{Question: "kind", Op: ShowIs, Values: []string{"Idea"}}},
		{Id: "why", Label: "Why it matters", Type: LongText, Required: true},
		{Id: "s3", Label: "Anything else", Type: Section},
		{Id: "areas", Label: "Areas", Type: MultiSelect, Options: []string{"Web", "Phone", "API"}},
		{Id: "site", Label: "Where it happens", Type: URL, ShowIf: &ShowIf{Question: "areas", Op: ShowAnyOf, Values: []string{"Web"}}},
		{Id: "phone", Label: "Phone", Type: Phone},
	}
}

func answered(t *testing.T, fields []Field, raw map[string]any) map[string]Answer {
	t.Helper()
	a, err := CheckAnswers(fields, raw, formula.NumberSigns{})
	if err != nil {
		t.Fatalf("%v: %v", raw, err)
	}
	out := map[string]Answer{}
	for _, x := range a {
		out[x.Id] = x
	}
	return out
}

// What a visitor is asked follows their answers, on the server too: a hidden
// question is never required, and an answer to one is dropped, whatever the
// page sent.
func TestABranchAsksOnlyWhatItsAnswersOpen(t *testing.T) {
	f := branching()
	if _, err := CheckAnswers(f, map[string]any{"what": "Crash", "kind": "Bug"}, formula.NumberSigns{}); err == nil {
		t.Error("a bug was sent without its steps")
	}
	bug := answered(t, f, map[string]any{"what": "Crash", "kind": "Bug", "steps": "Open it", "why": "sneaked in"})
	if bug["steps"].Value != "Open it" {
		t.Errorf("the steps were lost: %+v", bug)
	}
	if _, ok := bug["why"]; ok {
		t.Error("an answer to a page the visitor never saw was kept")
	}
	if _, err := CheckAnswers(f, map[string]any{"what": "Dark mode", "kind": "Idea"}, formula.NumberSigns{}); err == nil {
		t.Error("an idea was sent without its page's required answer")
	}
	idea := answered(t, f, map[string]any{"what": "Dark mode", "kind": "Idea", "why": "Night shifts", "steps": "sneaked in"})
	if _, ok := idea["steps"]; ok || idea["why"].Value != "Night shifts" {
		t.Errorf("idea %+v", idea)
	}
	web := answered(t, f, map[string]any{"what": "x", "kind": "Bug", "steps": "y", "areas": []any{"Web", "API"}, "site": "https://example.com/app"})
	if web["areas"].Value != "Web, API" || len(web["areas"].Values) != 2 || web["site"].Value != "https://example.com/app" {
		t.Errorf("web %+v", web)
	}
	phone := answered(t, f, map[string]any{"what": "x", "kind": "Bug", "steps": "y", "areas": []any{"Phone"}, "site": "https://example.com"})
	if _, ok := phone["site"]; ok {
		t.Error("a link was kept from someone the web question never showed to")
	}
}

// The new kinds read their answers strictly: several choices from the list,
// a web link, and a phone number.
func TestMoreKindsOfQuestion(t *testing.T) {
	f := branching()
	base := func(k string, v any) map[string]any {
		return map[string]any{"what": "x", "kind": "Bug", "steps": "y", "areas": []any{"Web"}, k: v}
	}
	for i, raw := range []map[string]any{
		base("areas", []any{"Desk"}),
		base("areas", "Web"),
		base("site", "ftp://example.com"),
		base("site", "javascript:alert(1)"),
		base("site", "example.com"),
		base("phone", "call me"),
		base("phone", "12"),
	} {
		if _, err := CheckAnswers(f, raw, formula.NumberSigns{}); err == nil {
			t.Errorf("case %d accepted: %v", i, raw)
		}
	}
	got := answered(t, f, base("phone", "+1 (555) 010-2000"))
	if got["phone"].Value != "+1 (555) 010-2000" {
		t.Errorf("phone %+v", got["phone"])
	}
	twice := answered(t, f, base("areas", []any{"Web", "Web", "API"}))
	if twice["areas"].Value != "Web, API" {
		t.Errorf("a choice ticked twice reads %q", twice["areas"].Value)
	}
}

// A form's branches are checked as it is saved: a question shows on an
// earlier answer only (so a form can't loop), on a choice it has, and a
// section has something after it.
func TestABranchIsCheckedWhenTheFormIsSaved(t *testing.T) {
	if _, err := CheckForm(Input{Title: "Requests", Fields: branching()}); err != nil {
		t.Fatalf("a good form was refused: %v", err)
	}
	q := func(id, typ string, show *ShowIf, opts ...string) Field {
		return Field{Id: id, Label: id, Type: typ, ShowIf: show, Options: opts}
	}
	is := func(on, v string) *ShowIf { return &ShowIf{Question: on, Op: ShowIs, Values: []string{v}} }
	bad := [][]Field{
		{q("a", ShortText, is("b", "Yes")), q("b", Checkbox, nil)},                                 // on a later answer
		{q("a", ShortText, is("a", "Yes"))},                                                        // on itself
		{q("a", ShortText, nil), q("b", ShortText, is("a", "x"))},                                  // "is" on a text answer
		{q("a", Select, nil, "One", "Two"), q("b", ShortText, is("a", "Three"))},                   // not a choice
		{q("a", Select, nil, "One", "Two"), q("b", ShortText, &ShowIf{Question: "a", Op: "near"})}, // an op it can't read
		{q("a", ShortText, nil), q("s", Section, nil)},                                             // a section with nothing after it
		{q("s", Section, nil), q("t", Section, nil), q("a", ShortText, nil)},                       // an empty section
		{{Id: "a", Label: "a", Type: ShortText, Help: strings.Repeat("h", 301)}},
	}
	for i, fields := range bad {
		if _, err := CheckForm(Input{Title: "x", Fields: fields}); err == nil {
			t.Errorf("case %d saved", i)
		}
	}
	answeredOnText := []Field{q("a", ShortText, nil), q("b", ShortText, &ShowIf{Question: "a", Op: ShowAnswered})}
	if _, err := CheckForm(Input{Title: "x", Fields: answeredOnText}); err != nil {
		t.Errorf(`"answered" on a text answer was refused: %v`, err)
	}
	// A section isn't a question: twenty questions and some sections save.
	many := []Field{q("s0", Section, nil)}
	for i := 0; i < 20; i++ {
		many = append(many, Field{Id: "q" + string(rune('a'+i)), Label: "Q", Type: ShortText})
	}
	if _, err := CheckForm(Input{Title: "x", Fields: many}); err != nil {
		t.Errorf("twenty questions under a section were refused: %v", err)
	}
	// A section never asks: it can't be required, and its answer isn't one.
	f, _ := CheckForm(Input{Title: "x", Fields: []Field{{Id: "s", Label: "Page", Type: Section, Required: true}, q("a", ShortText, nil)}})
	var stored []Field
	_ = json.Unmarshal(f.Fields, &stored)
	if stored[0].Required || f.TitleField != "a" {
		t.Errorf("stored %+v, names its task from %q", stored, f.TitleField)
	}
}

// What an answer fills is checked as the form is saved: a choice sets the
// priority only when each of its choices says which, a date sets the due
// date, choices set tags, and one question at most sets each of them.
func TestWhatAnAnswerFillsIsChecked(t *testing.T) {
	sel := func(m map[string]string) Field {
		return Field{Id: "p", Label: "How urgent?", Type: Select, Options: []string{"Now", "Later"}, MapsTo: MapPriority, Map: m}
	}
	ok := []Field{
		{Id: "what", Label: "What", Type: ShortText},
		sel(map[string]string{"Now": "high", "Later": "low"}),
		{Id: "by", Label: "Needed by", Type: Date, MapsTo: MapDue},
		{Id: "areas", Label: "Areas", Type: MultiSelect, Options: []string{"Web", "API"}, MapsTo: MapTags},
		{Id: "f", Label: "Budget", Type: Number, MapsTo: "field:" + uuid.NewString()},
	}
	if _, err := CheckForm(Input{Title: "x", Fields: ok}); err != nil {
		t.Fatalf("a good mapping was refused: %v", err)
	}
	bad := [][]Field{
		{{Id: "a", Label: "a", Type: ShortText, MapsTo: MapPriority}},
		{sel(map[string]string{"Now": "high"})},
		{sel(map[string]string{"Now": "high", "Later": "someday"})},
		{{Id: "a", Label: "a", Type: ShortText, MapsTo: MapDue}},
		{{Id: "a", Label: "a", Type: Date, MapsTo: MapTags}},
		{{Id: "a", Label: "a", Type: Date, MapsTo: MapDue}, {Id: "b", Label: "b", Type: Date, MapsTo: MapDue}},
		{{Id: "a", Label: "a", Type: Number, MapsTo: "field:nope"}},
		{{Id: "a", Label: "a", Type: Number, MapsTo: "status"}},
		{{Id: "s", Label: "Page", Type: Section, MapsTo: MapTags}, {Id: "a", Label: "a", Type: ShortText}},
	}
	for i, fields := range bad {
		if _, err := CheckForm(Input{Title: "x", Fields: fields}); err == nil {
			t.Errorf("case %d saved", i)
		}
	}
}

// A form's answers fill the task: its priority, its due date (that day's
// midnight where the visitor is), its tags, and the project's own fields,
// each by its kind.
func TestAnswersFillTheTask(t *testing.T) {
	text, num, money, day, pick, many, tick, link :=
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	project := []*taskFieldModel.Field{
		{ID: text, Name: "Team", Type: taskFieldModel.TypeText},
		{ID: num, Name: "Seats", Type: taskFieldModel.TypeNumber},
		{ID: money, Name: "Budget", Type: taskFieldModel.TypeMoney},
		{ID: day, Name: "Launch", Type: taskFieldModel.TypeDate},
		{ID: pick, Name: "Stage", Type: taskFieldModel.TypeSelect, Options: []taskFieldModel.Option{{ID: "o1", Label: "Draft"}, {ID: "o2", Label: "Live"}}},
		{ID: many, Name: "Channels", Type: taskFieldModel.TypeMultiSelect, Options: []taskFieldModel.Option{{ID: "c1", Label: "Email"}, {ID: "c2", Label: "Ads"}}},
		{ID: tick, Name: "Signed", Type: taskFieldModel.TypeCheckbox},
		{ID: link, Name: "Brief", Type: taskFieldModel.TypeURL},
	}
	to := func(id uuid.UUID) string { return "field:" + id.String() }
	fields := []Field{
		{Id: "what", Label: "What", Type: ShortText},
		{Id: "p", Label: "How urgent?", Type: Select, Options: []string{"Now", "Later"}, MapsTo: MapPriority, Map: map[string]string{"Now": "high", "Later": "low"}},
		{Id: "by", Label: "Needed by", Type: Date, MapsTo: MapDue},
		{Id: "areas", Label: "Areas", Type: MultiSelect, Options: []string{"Web", "API"}, MapsTo: MapTags},
		{Id: "team", Label: "Team", Type: Email, MapsTo: to(text)},
		{Id: "seats", Label: "Seats", Type: Number, MapsTo: to(num)},
		{Id: "budget", Label: "Budget", Type: Number, MapsTo: to(money)},
		{Id: "launch", Label: "Launch", Type: Date, MapsTo: to(day)},
		{Id: "stage", Label: "Stage", Type: Select, Options: []string{"Draft", "Live"}, MapsTo: to(pick)},
		{Id: "chan", Label: "Channels", Type: MultiSelect, Options: []string{"Email", "Ads", "TV"}, MapsTo: to(many)},
		{Id: "signed", Label: "Signed", Type: Checkbox, MapsTo: to(tick)},
		{Id: "brief", Label: "Brief", Type: URL, MapsTo: to(link)},
	}
	de := formula.NumberSigns{Decimal: ",", Group: "."}
	answers, err := CheckAnswers(fields, map[string]any{
		"what": "Launch page", "p": "Now", "by": "2026-11-02", "areas": []any{"Web", "API"}, "team": "ana@example.com",
		"seats": "1.200", "budget": "49,99", "launch": "2026-12-01", "stage": "Live", "chan": []any{"Ads", "TV"},
		"signed": true, "brief": "https://example.com/brief",
	}, de)
	if err != nil {
		t.Fatal(err)
	}
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	v := Fill(fields, answers, kolkata, project)
	if v.Priority != "high" || v.Due == nil || !v.Due.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, kolkata)) {
		t.Errorf("priority %q, due %v", v.Priority, v.Due)
	}
	if strings.Join(v.Tags, ",") != "Web,API" {
		t.Errorf("tags %v", v.Tags)
	}
	want := map[uuid.UUID]string{text: `"ana@example.com"`, num: `1200`, money: `4999`, day: `"2026-12-01"`, pick: `"o2"`,
		many: `["c2"]`, tick: `true`, link: `"https://example.com/brief"`}
	for id, w := range want {
		if got := string(v.Fields[id]); got != w {
			t.Errorf("field %s = %s, want %s", id, got, w)
		}
	}
	// A choice the field no longer has is left out, not refused: the answer
	// is still in the description.
	answers, _ = CheckAnswers(fields, map[string]any{"stage": "Draft", "chan": []any{"TV"}}, formula.NumberSigns{})
	project[4].Options = []taskFieldModel.Option{{ID: "o2", Label: "Live"}}
	v = Fill(fields, answers, time.UTC, project)
	if _, ok := v.Fields[pick]; ok {
		t.Errorf("a choice the field doesn't have was written: %s", v.Fields[pick])
	}
	if _, ok := v.Fields[many]; ok {
		t.Errorf("no choice the field has was written as %s", v.Fields[many])
	}
}

// The task's name is the answer to its question, by the question's id: two
// questions may share a label.
func TestATaskIsNamedByItsQuestionsId(t *testing.T) {
	fields := []Field{{Id: "a", Label: "Name", Type: ShortText}, {Id: "b", Label: "Name", Type: ShortText}}
	name, _ := TaskFrom("Requests", fields, "b", []Answer{{Id: "a", Label: "Name", Value: "first"}, {Id: "b", Label: "Name", Value: "second"}})
	if name != "second" {
		t.Errorf("named %q", name)
	}
	stored, _ := json.Marshal([]Answer{{Id: "a", Label: "Name", Value: "first"}})
	if !strings.Contains(string(stored), `"id":"a"`) {
		t.Errorf("answers are stored without their question: %s", stored)
	}
	var old []Answer
	_ = json.Unmarshal([]byte(`[{"Label":"Name","Value":"kept"}]`), &old)
	if old[0].Label != "Name" || old[0].Value != "kept" {
		t.Errorf("an old submission reads %+v", old)
	}
}
