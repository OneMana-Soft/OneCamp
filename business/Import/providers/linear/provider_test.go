package linear

// Tests for the Linear provider's pure helper logic. The GraphQL
// fetchers and cache are exercised by the integration suite (which
// mocks Linear's API behind a httptest server). Anything that doesn't
// need network or DB lives here.

import (
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

func TestMarkdownToHTML_BasicShape(t *testing.T) {
	in := "Hello\n\nWorld"
	got := markdownToHTML(in)
	// Two paragraphs.
	if !strings.Contains(got, "<p>Hello</p>") || !strings.Contains(got, "<p>World</p>") {
		t.Fatalf("expected 2 paragraphs, got %q", got)
	}
}

func TestMarkdownToHTML_HtmlEscape(t *testing.T) {
	in := `<script>alert("xss")</script>`
	got := markdownToHTML(in)
	if strings.Contains(got, "<script>") {
		t.Fatalf("expected raw <script> to be escaped, got %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("expected escaped tag, got %q", got)
	}
}

func TestMarkdownToHTML_SingleLineBreak(t *testing.T) {
	in := "line one\nline two"
	got := markdownToHTML(in)
	if !strings.Contains(got, "line one<br/>line two") {
		t.Fatalf("expected single newline → <br/>, got %q", got)
	}
}

func TestTruncate_NoChangeUnderLimit(t *testing.T) {
	if truncate("abc", 10) != "abc" {
		t.Fatal("under-limit string should pass through")
	}
}

func TestTruncate_CutsAtLimit(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc" {
		t.Fatalf("expected abc, got %q", got)
	}
}

func TestParseRetryAfter_Numeric(t *testing.T) {
	if got := parseRetryAfter("30"); got != 30 {
		t.Fatalf("expected 30, got %d", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Fatalf("expected 0 for empty, got %d", got)
	}
}

func TestIssueToSourceTask_Basic(t *testing.T) {
	p := New()
	now := time.Now().UTC()
	completed := now.Add(-1 * time.Hour)
	iss := linearIssue{
		ID:            "iss-1",
		Identifier:    "ENG-42",
		Title:         "Fix the navbar",
		Description:   "**bold**",
		URL:           "https://linear.app/acme/issue/ENG-42",
		Priority:      2,
		PriorityLabel: "High",
		CreatedAt:     now.Add(-24 * time.Hour),
		UpdatedAt:     now,
		CompletedAt:   &completed,
		StateName:     "Done",
		StateType:     "completed",
		AssigneeID:    "user-1",
		CreatorID:     "user-2",
		ProjectID:     "proj-1",
		TeamID:        "team-1",
		Labels:        []string{"bug"},
		Comments:      []linearComment{{ID: "c1"}, {ID: "c2"}},
		Attachments:   []linearAttachment{{ID: "a1", Name: "screenshot.png", URL: "https://uploads.linear.app/x.png"}},
	}

	out := p.issueToSourceTask(iss, "proj-1")

	if out.SourceID != "iss-1" {
		t.Fatalf("source id mismatch: %q", out.SourceID)
	}
	if out.Status != "Done" {
		t.Fatalf("expected Status=Done, got %q", out.Status)
	}
	if out.Priority != "High" {
		t.Fatalf("expected priority High, got %q", out.Priority)
	}
	if out.Completed != true {
		t.Fatal("expected Completed=true when CompletedAt is set")
	}
	if out.CommentCount != 2 {
		t.Fatalf("expected CommentCount=2, got %d", out.CommentCount)
	}
	if len(out.AttachmentRefs) != 1 || out.AttachmentRefs[0].URL != "https://uploads.linear.app/x.png" {
		t.Fatalf("attachment didn't pass through: %+v", out.AttachmentRefs)
	}
	if out.Metadata["linear_identifier"] != "ENG-42" {
		t.Fatalf("expected metadata.linear_identifier=ENG-42, got %v", out.Metadata["linear_identifier"])
	}
}

func TestIssueToSourceTask_NoPriorityFallback(t *testing.T) {
	p := New()
	iss := linearIssue{
		ID:            "iss-2",
		Title:         "no priority issue",
		Priority:      0,
		PriorityLabel: "", // Linear returns empty for priority=0
	}
	out := p.issueToSourceTask(iss, "p1")
	if out.Priority != "no priority" {
		t.Fatalf("expected fallback 'no priority', got %q", out.Priority)
	}
}

func TestCapabilities_HasExpectedFlags(t *testing.T) {
	p := New()
	caps := p.Capabilities()
	for _, want := range []importProvider.Capability{
		importProvider.CapTeams,
		importProvider.CapProjects,
		importProvider.CapTasks,
		importProvider.CapSubtasks,
		importProvider.CapTaskComments,
		importProvider.CapAttachments,
	} {
		if caps&want == 0 {
			t.Fatalf("expected capability bit %d to be set", want)
		}
	}
}

func TestDefaultStatusMap_LowerCaseKeys(t *testing.T) {
	p := New()
	for k := range p.DefaultStatusMap() {
		if k != strings.ToLower(k) {
			t.Fatalf("status map key %q is not lower-case", k)
		}
	}
}

func TestDefaultPriorityMap_LowerCaseKeys(t *testing.T) {
	p := New()
	for k := range p.DefaultPriorityMap() {
		if k != strings.ToLower(k) {
			t.Fatalf("priority map key %q is not lower-case", k)
		}
	}
}

// TestDecodeTeamScope_Variants covers the option decoder used to
// narrow imports to a single Linear team.
func TestDecodeTeamScope_Variants(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"no team_id key", `{"other":"value"}`, ""},
		{"with team_id", `{"team_id":"team-abc"}`, "team-abc"},
		{"empty team_id", `{"team_id":""}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := &importModelsAlias{}
			if tc.raw != "" {
				j.Options = []byte(tc.raw)
			}
			got, err := decodeTeamScope(asJob(j))
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// importModelsAlias is a tiny shim so the test file doesn't have to
// import importModels in two ways. asJob casts back to the real
// type expected by decodeTeamScope.
type importModelsAlias struct {
	Options []byte
}

// asJob constructs a minimal *importModels.Job around our alias for
// the helper to consume. We only set Options because that's what
// decodeTeamScope inspects.
func asJob(a *importModelsAlias) *importModels.Job {
	if a == nil {
		return nil
	}
	return &importModels.Job{Options: a.Options}
}
