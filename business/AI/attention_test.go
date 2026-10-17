package business

import (
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// sortAttention must order by urgency tier first, then earliest due date within
// a tier, with dated items ahead of undated ones in the same tier.
func TestSortAttention(t *testing.T) {
	items := []adapter.AttentionItem{
		{Source: "question", Priority: prQuestion, Title: "q1"},
		{Source: "task", Priority: prOverdue, DueAt: "2026-06-20", Title: "later"},
		{Source: "approval", Priority: prApproval, Title: "approve me"},
		{Source: "task", Priority: prOverdue, DueAt: "2026-06-10", Title: "earlier"},
		{Source: "calendar", Priority: prCalendar, Title: "standup"},
		{Source: "commitment", Priority: prOverdue, Title: "undated commitment"},
	}

	sortAttention(items)

	// Tier order: approval(0) < overdue(1)x3 < calendar(2) < question(3).
	order := []string{
		"approve me",         // approval
		"earlier",            // overdue, due 06-10
		"later",              // overdue, due 06-20
		"undated commitment", // overdue, undated (after dated)
		"standup",            // calendar
		"q1",                 // question
	}
	if len(items) != len(order) {
		t.Fatalf("expected %d items, got %d", len(order), len(items))
	}
	for i, want := range order {
		if items[i].Title != want {
			t.Fatalf("position %d = %q, want %q (full: %+v)", i, items[i].Title, want, titles(items))
		}
	}
}

// Within the same tier, a dated item must sort before an undated one.
func TestSortAttentionDatedBeforeUndated(t *testing.T) {
	items := []adapter.AttentionItem{
		{Priority: prOverdue, Title: "undated"},
		{Priority: prOverdue, DueAt: "2026-01-01", Title: "dated"},
	}
	sortAttention(items)
	if items[0].Title != "dated" {
		t.Fatalf("dated item should sort first, got %q", items[0].Title)
	}
}

func titles(items []adapter.AttentionItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

func TestTaskToAttentionItem_OverdueVsDueSoon(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	past := now.Add(-48 * time.Hour)
	overdue := taskToAttentionItem(&dgraphStruct.DgraphTask{Name: "Ship it", Uuid: "t1", DueDate: &past}, now)
	if overdue.Priority != prOverdue || overdue.Kind != "Overdue task" {
		t.Fatalf("expected overdue classification, got %+v", overdue)
	}
	if overdue.URL != "/app/task/t1" {
		t.Fatalf("expected task deep link, got %q", overdue.URL)
	}

	soon := now.Add(3 * time.Hour)
	dueSoon := taskToAttentionItem(&dgraphStruct.DgraphTask{Name: "Review PR", Uuid: "t2", DueDate: &soon}, now)
	if dueSoon.Priority != prDueSoon || dueSoon.Kind != "Due soon" {
		t.Fatalf("expected due-soon classification, got %+v", dueSoon)
	}
	// Overdue must rank ahead of due-soon.
	if !(overdue.Priority < dueSoon.Priority) {
		t.Fatalf("overdue should outrank due-soon: %d vs %d", overdue.Priority, dueSoon.Priority)
	}
}

// The exact due moment travels as UTC, so the client can say it in the viewer's
// own zone; the server's zone is not the viewer's.
func TestTaskAttentionCarriesTheExactDueTimeAndItsProject(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	due := time.Date(2026, 9, 24, 17, 0, 0, 0, ist)
	it := taskToAttentionItem(&dgraphStruct.DgraphTask{Name: "Write it", Uuid: "t3", DueDate: &due,
		Project: &dgraphStruct.DgraphProject{Name: " Q4 launch "}}, due.Add(-time.Hour))
	if it.DueTime != "2026-09-24T11:30:00Z" {
		t.Fatalf("due_time = %q, want the same instant in UTC", it.DueTime)
	}
	if it.Context != "Q4 launch" {
		t.Fatalf("context = %q", it.Context)
	}
	undated := taskToAttentionItem(&dgraphStruct.DgraphTask{Name: "Someday"}, due)
	if undated.DueTime != "" {
		t.Fatalf("an undated task has a due time: %q", undated.DueTime)
	}
}

// A task highlight keeps its name and description apart; flattened, they ran together.
func TestATaskHighlightSeparatesItsNameFromItsDescription(t *testing.T) {
	got := highlightText("task", "Write the launch announcement\n\nDraft for the blog.")
	if got != "Write the launch announcement · Draft for the blog." {
		t.Fatalf("got %q", got)
	}
	if got := highlightText("task", "Just a name"); got != "Just a name" {
		t.Fatalf("a task with no description: %q", got)
	}
	if got := highlightText("post", "first\n\nsecond"); strings.Contains(got, "·") {
		t.Fatalf("only tasks and docs are split: %q", got)
	}
}

// QA SH-09 (14 Oct 2026): a doc's title and body ran together in the
// briefing, "Launch sync notes Attendees: Sam, Maya, Jonas". A highlight with a
// name of its own sends it apart from the text under it.
func TestADocHighlightSendsItsTitleApartFromItsBody(t *testing.T) {
	if got := highlightText("doc", "Launch sync notes\n\nAttendees: Sam, Maya, Jonas"); got != "Launch sync notes · Attendees: Sam, Maya, Jonas" {
		t.Fatalf("snippet: %q", got)
	}
	h := toBriefingHighlight(ai.SimilarResult{ContentType: "doc", ContentUUID: "d1", ContentText: "Launch sync notes\n\n<p>Attendees: Sam, Maya, Jonas</p>"})
	if h.Title != "Launch sync notes" || h.Excerpt != "Attendees: Sam, Maya, Jonas" {
		t.Fatalf("title %q, excerpt %q", h.Title, h.Excerpt)
	}
	for _, r := range []ai.SimilarResult{
		{ContentType: "post", ContentText: "first\n\nsecond"},
		{ContentType: "doc", ContentText: "A title alone"},
	} {
		if h := toBriefingHighlight(r); h.Title != "" || h.Excerpt != "" {
			t.Fatalf("%s %q: title %q, excerpt %q", r.ContentType, r.ContentText, h.Title, h.Excerpt)
		}
	}
	long := strings.Repeat("x", 300)
	if h := toBriefingHighlight(ai.SimilarResult{ContentType: "task", ContentText: "Name\n\n" + long}); len([]rune(h.Excerpt)) > 161 {
		t.Fatalf("excerpt not cut: %d runes", len([]rune(h.Excerpt)))
	}
}
