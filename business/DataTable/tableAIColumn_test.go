package business

import (
	"strings"
	"testing"

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
