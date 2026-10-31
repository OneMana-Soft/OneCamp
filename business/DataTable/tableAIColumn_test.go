package business

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestAIPromptFromConfig(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"not an ai column", `{"options":["a","b"]}`, ""},
		{"ai column", `{"ai":{"prompt":"Summarize the row"}}`, "Summarize the row"},
		{"ai column trims", `{"ai":{"prompt":"  hello  "}}`, "hello"},
		{"ai empty prompt", `{"ai":{"prompt":""}}`, ""},
		{"malformed json", `{not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := aiPromptFromConfig(c.config); got != c.want {
				t.Fatalf("aiPromptFromConfig(%q) = %q, want %q", c.config, got, c.want)
			}
		})
	}
}

func TestAIConfigFromConfig(t *testing.T) {
	cases := []struct {
		name       string
		config     string
		wantPrompt string
		wantAuto   bool
	}{
		{"empty", "", "", false},
		{"not ai", `{"options":["a"]}`, "", false},
		{"ai no auto", `{"ai":{"prompt":"Summarize"}}`, "Summarize", false},
		{"ai auto false", `{"ai":{"prompt":"Summarize","auto":false}}`, "Summarize", false},
		{"ai auto true", `{"ai":{"prompt":"Summarize","auto":true}}`, "Summarize", true},
		{"auto true empty prompt", `{"ai":{"prompt":"","auto":true}}`, "", true},
		{"malformed", `{bad`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPrompt, gotAuto := aiConfigFromConfig(c.config)
			if gotPrompt != c.wantPrompt || gotAuto != c.wantAuto {
				t.Fatalf("aiConfigFromConfig(%q) = (%q,%v), want (%q,%v)", c.config, gotPrompt, gotAuto, c.wantPrompt, c.wantAuto)
			}
		})
	}
}

func TestRenderCellValue(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"nil", nil, ""},
		{"string", "hi", "hi"},
		{"bool true", true, "yes"},
		{"bool false", false, "no"},
		{"int-like float", float64(42), "42"},
		{"decimal float", 3.5, "3.5"},
		{"slice", []interface{}{"a", "", "b"}, "a, b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderCellValue(c.in); got != c.want {
				t.Fatalf("renderCellValue(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestBuildCellPrompt(t *testing.T) {
	titleID := uuid.New()
	statusID := uuid.New()
	aiID := uuid.New()      // the target AI column
	otherAIID := uuid.New() // another AI column, must be excluded from context

	fields := []*model.Field{
		{Id: titleID, Name: "Title", Config: ""},
		{Id: statusID, Name: "Status", Config: `{"options":["open","done"]}`},
		{Id: aiID, Name: "Summary", Config: `{"ai":{"prompt":"Summarize"}}`},
		{Id: otherAIID, Name: "Sentiment", Config: `{"ai":{"prompt":"Sentiment"}}`},
	}

	valuesJSON := `{
		"` + titleID.String() + `": "Ship the login fix",
		"` + statusID.String() + `": "open",
		"` + otherAIID.String() + `": "positive"
	}`

	out := buildCellPrompt("Write a one-line summary", fields, valuesJSON, aiID.String())

	if !strings.Contains(out, "Write a one-line summary") {
		t.Fatalf("prompt missing column instruction: %q", out)
	}
	if !strings.Contains(out, "Title: Ship the login fix") {
		t.Fatalf("prompt missing non-AI column context: %q", out)
	}
	if !strings.Contains(out, "Status: open") {
		t.Fatalf("prompt missing status context: %q", out)
	}
	// Another AI column must NOT be fed in as context.
	if strings.Contains(out, "Sentiment:") {
		t.Fatalf("prompt should not include other AI column context: %q", out)
	}
	// The target column's own value (if any) is excluded; ensure no self-feed.
	if strings.Contains(out, "Summary:") {
		t.Fatalf("prompt should not include the target column itself: %q", out)
	}
}

func TestBuildCellPromptBlankValues(t *testing.T) {
	id := uuid.New()
	fields := []*model.Field{{Id: id, Name: "Notes", Config: ""}}
	out := buildCellPrompt("Do the thing", fields, "", uuid.New().String())
	if !strings.Contains(out, "Do the thing") {
		t.Fatalf("prompt missing instruction: %q", out)
	}
	// No "Row data:" section when there are no other populated columns.
	if strings.Contains(out, "Row data:") {
		t.Fatalf("did not expect row data section: %q", out)
	}
}

func TestBuildCellPromptBudget(t *testing.T) {
	long := strings.Repeat("x", aiCellPromptBudget+500)
	out := buildCellPrompt(long, nil, "", uuid.New().String())
	if len(out) > aiCellPromptBudget {
		t.Fatalf("prompt exceeds budget: %d > %d", len(out), aiCellPromptBudget)
	}
}

// An auto AI column is asked again only when a cell its prompt reads changed,
// never when the write set its own cell, and never for a cell a person wrote
// (TAB-BUG-4).
func TestAutofillAsksOnlyWhenWhatItReadsChanged(t *testing.T) {
	field := func(name, typ, cfg string) *model.Field {
		return &model.Field{Id: uuid.New(), Name: name, Type: typ, Config: cfg}
	}
	notes := field("Notes", model.FieldText, `{}`)
	status := field("Status", model.FieldSelect, `{}`)
	length := field("Notes length", model.FieldFormula, fmt.Sprintf(`{"formula":"LEN({#%s})"}`, notes.Id))
	summary := field("Summary", model.FieldText, `{"ai":{"prompt":"Summarise {Notes} in a line","auto":true}}`)
	rating := field("Rating", model.FieldText, `{"ai":{"prompt":"Rate how long {notes length} is","auto":true}}`)
	tagline := field("Tagline", model.FieldText, `{"ai":{"prompt":"Write a tagline for this row","auto":true}}`)
	draft := field("Draft", model.FieldText, `{"ai":{"prompt":"Draft a reply to {Notes}"}}`)
	fields := []*model.Field{notes, status, length, summary, rating, tagline, draft}
	due := func(row *model.Row, changed ...*model.Field) string {
		set := map[string]bool{}
		for _, f := range changed {
			set[f.Id.String()] = true
		}
		var names []string
		for _, f := range autofillDue(fields, row, set) {
			names = append(names, f.Name)
		}
		return strings.Join(names, ", ")
	}
	row := &model.Row{}
	cases := []struct {
		name    string
		row     *model.Row
		changed []*model.Field
		want    string
	}{
		{"a cell every prompt reads", row, []*model.Field{notes}, "Summary, Rating, Tagline"},
		{"a cell only the prompt naming no field reads", row, []*model.Field{status}, "Tagline"},
		{"a person's write to an AI cell asks nothing", row, []*model.Field{summary}, ""},
		{"a cell a person wrote stays theirs", &model.Row{AIKept: []string{summary.Id.String()}}, []*model.Field{notes}, "Rating, Tagline"},
		{"the write that set an AI cell doesn't fill it", row, []*model.Field{notes, summary}, "Rating, Tagline"},
		{"a write that changed nothing", row, nil, ""},
	}
	for _, c := range cases {
		if got := due(c.row, c.changed...); got != c.want {
			t.Errorf("%s: asks %q, want %q", c.name, got, c.want)
		}
	}
}

func TestChangedCells(t *testing.T) {
	was := `{"a": "x", "b": [1, 2], "c": {"k": 1}, "gone": true, "_ai_kept": ["z"]}`
	is := `{"a":"x","b":[1,2],"c":{"k":2},"new":null}`
	got := changedCells(was, is)
	want := map[string]bool{"c": true, "gone": true, "new": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
	if got := changedCells("{}", `{"a": "x"}`); !reflect.DeepEqual(got, map[string]bool{"a": true}) {
		t.Errorf("a new row's cells: %v", got)
	}
	// A row with a person's AI cell is written again as its mark is lifted
	// out, with "&" escaped: the same cells, not changed ones.
	if got := changedCells(`{"url": "https://x.io/?a=1&b=2", "n": 1.50, "_ai_kept": ["f"]}`, `{"n":1.50,"url":"https://x.io/?a=1\u0026b=2"}`); len(got) != 0 {
		t.Errorf("the same cells written again read as changed: %v", got)
	}
}

// An AI column's answer is written only while the cells it's worked out from
// are as the fill read them: those its prompt names, or, naming none, every
// cell but the AI columns', as the row stores them (null where it had none).
func TestReadCells(t *testing.T) {
	field := func(name, typ, config string) *model.Field {
		return &model.Field{Id: uuid.New(), Name: name, Type: typ, Config: config}
	}
	notes := field("Notes", model.FieldText, "{}")
	stage := field("Stage", model.FieldSelect, "{}")
	price := field("Price", model.FieldNumber, "{}")
	twice := field("Twice", model.FieldFormula, `{"formula":"{#`+price.Id.String()+`} * 2"}`)
	summary := field("Summary", model.FieldText, `{"ai":{"prompt":"Summarise {Notes}","auto":true}}`)
	fields := []*model.Field{notes, stage, price, twice, summary}
	rw := &model.Row{Values: `{"` + notes.Id.String() + `": "Renewal due in July", "` + price.Id.String() + `": 12, "` +
		twice.Id.String() + `": 24, "` + summary.Id.String() + `": "Renews in July"}`}
	cells := func(prompt string) map[string]interface{} {
		var got map[string]interface{}
		if err := json.Unmarshal([]byte(readCells(fields, rw, prompt)), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := cells("Summarise {Notes}"); !reflect.DeepEqual(got, map[string]interface{}{notes.Id.String(): "Renewal due in July"}) {
		t.Errorf("a prompt naming Notes read %v", got)
	}
	if got := cells("Double check {Twice}"); !reflect.DeepEqual(got, map[string]interface{}{price.Id.String(): 12.0}) {
		t.Errorf("a prompt naming a formula read %v, want the cell the formula reads", got)
	}
	want := map[string]interface{}{notes.Id.String(): "Renewal due in July", stage.Id.String(): nil, price.Id.String(): 12.0}
	if got := cells("Summarise the row"); !reflect.DeepEqual(got, want) {
		t.Errorf("a prompt naming nothing read %v, want %v", got, want)
	}
}

// A table drafted by the AI is private to the person who asked: its rows can
// quote what only they can read (TAB-BUG-6).
func TestADraftedTableIsPrivate(t *testing.T) {
	gen, err := parseGeneratedTable(`{"name":"Plans","fields":[{"name":"Plan","type":"text"},{"name":"Stage","type":"select","options":["Now","Later"]}],
		"rows":[{"Plan":"Cut the Pune office in Q4","Stage":"Now"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	in, fields, err := draftInputs(gen)
	if err != nil || in.Visibility != model.VisibilityPrivate || len(fields) != 2 {
		t.Errorf("the draft makes %+v with %d columns (%v)", in, len(fields), err)
	}
}

// A drafted table's name is cut to 120 characters, never inside one: a cut by
// bytes split a letter of a long Hindi name, and the table couldn't be stored.
// (Here, in the AI-only tests, as the draft is AI-only.)
func TestADraftedTableNameIsCutByCharacters(t *testing.T) {
	g, err := parseGeneratedTable(`{"name":"` + strings.Repeat("क", maxNameLen+10) + `","fields":[{"name":"Item","type":"text"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(g.Name) || utf8.RuneCountInString(g.Name) != maxNameLen {
		t.Errorf("the name was cut to %d characters, valid UTF-8 %v", utf8.RuneCountInString(g.Name), utf8.ValidString(g.Name))
	}
}
