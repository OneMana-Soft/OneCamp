package business

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseProposedTasks(t *testing.T) {
	raw := "```json\n" + `[
      {"title": "Draft the pricing doc", "assignee": "Alice", "due": "2026-07-03", "priority": "high", "details": "for the launch"},
      {"title": "", "assignee": "Bob"},
      {"title": "Follow up on vendor contract", "priority": "weird"}
    ]` + "\n```"
	out := parseProposedTasks(raw)
	if len(out) != 2 {
		t.Fatalf("expected 2 tasks (empty-title dropped), got %d", len(out))
	}
	if out[0].Title != "Draft the pricing doc" || out[0].Assignee != "Alice" || out[0].Due != "2026-07-03" {
		t.Fatalf("unexpected first task: %+v", out[0])
	}
	if out[1].Title != "Follow up on vendor contract" {
		t.Fatalf("unexpected second task: %+v", out[1])
	}
}

func TestParseProposedTasks_Empty(t *testing.T) {
	if got := parseProposedTasks("[]"); len(got) != 0 {
		t.Fatalf("empty array should yield no tasks, got %d", len(got))
	}
	if got := parseProposedTasks("not json"); got != nil {
		t.Fatalf("non-JSON should yield nil, got %v", got)
	}
}

func TestParseProposedTasks_Cap(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < extractMaxTasks+5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"title":"t"}`)
	}
	b.WriteString("]")
	if got := parseProposedTasks(b.String()); len(got) != extractMaxTasks {
		t.Fatalf("expected cap at %d, got %d", extractMaxTasks, len(got))
	}
}

func TestNormalizeDueDate(t *testing.T) {
	if got := normalizeDueDate(""); got != "" {
		t.Fatalf("empty should stay empty, got %q", got)
	}
	if got := normalizeDueDate("nonsense"); got != "" {
		t.Fatalf("unparseable should be empty, got %q", got)
	}
	got := normalizeDueDate("2026-07-03")
	want := time.Date(2026, 7, 3, 17, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if got != want {
		t.Fatalf("date-only should map to 17:00 UTC, got %q want %q", got, want)
	}
	// RFC3339 passes through (normalized to UTC).
	if got := normalizeDueDate("2026-07-03T09:30:00Z"); got != "2026-07-03T09:30:00Z" {
		t.Fatalf("RFC3339 should pass through, got %q", got)
	}
}

// TestMeetingRoomAccessible covers the pure permission boundary for pulling a
// call room's transcript: channel/group membership (from the access graph)
// and DM participation (the requester must be one of the room's two UUIDs).
func TestMeetingRoomAccessible(t *testing.T) {
	me := uuid.New()
	peer := uuid.New()
	stranger := uuid.New()

	channelUUID := uuid.New().String()               // hyphenated UUID → channel room
	groupID := "abcdef0123456789abcdef0123456789"    // 32-char hex, no hyphen → group room
	otherGroup := "00112233445566778899aabbccddeeff" // group the user isn't in

	// User is in channelUUID and groupID.
	user := makeUser(me, []string{channelUUID}, nil, []string{groupID})

	cases := []struct {
		name string
		room string
		want bool
	}{
		{"channel member", channelUUID, true},
		{"channel non-member", uuid.New().String(), false},
		{"group member", groupID, true},
		{"group non-member", otherGroup, false},
		{"dm participant (self first)", me.String() + " " + peer.String(), true},
		{"dm participant (self second)", peer.String() + " " + me.String(), true},
		{"dm non-participant", peer.String() + " " + stranger.String(), false},
		{"empty room", "", false},
		{"whitespace room", "   ", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := meetingRoomAccessible(user, tc.room); got != tc.want {
				t.Fatalf("meetingRoomAccessible(%q) = %v, want %v", tc.room, got, tc.want)
			}
		})
	}
}

// A request in chat is usually a question ("can we get X in by Thursday?").
// The prompt once told the model to ignore questions, and a message asking for
// work came back with "No clear action items found".
func TestTaskExtractionPromptKeepsRequestsPhrasedAsQuestions(t *testing.T) {
	if !strings.Contains(taskExtractionPrompt, "phrased as questions") {
		t.Fatal("the extraction prompt must say a request phrased as a question is an action item")
	}
	if strings.Contains(taskExtractionPrompt, "opinions, and questions.") {
		t.Fatal("the extraction prompt must not tell the model to ignore every question")
	}
}

// JSON mode (response_format json_object) forbids a top-level array, so the
// answer comes back as an object. Before the prompt asked for {"tasks": [...]},
// a single action item came back as the bare item and was dropped.
func TestParseProposedTasks_Shapes(t *testing.T) {
	cases := map[string]struct {
		raw   string
		want  []string
		valid bool
	}{
		"wrapped":       {`{"tasks": [{"title": "Add the rollback steps"}, {"title": "Send the numbers"}]}`, []string{"Add the rollback steps", "Send the numbers"}, true},
		"wrapped empty": {`{"tasks": []}`, nil, true},
		"lone item":     {`{"title": "Send the Q4 numbers to finance", "due": "2026-10-02"}`, []string{"Send the Q4 numbers to finance"}, true},
		"bare array":    {`[{"title": "Draft the spec"}]`, []string{"Draft the spec"}, true},
		"fenced":        {"```json\n{\"tasks\": [{\"title\": \"Book the room\"}]}\n```", []string{"Book the room"}, true},
		"other object":  {`{"answer": "none"}`, nil, false},
		"prose":         {"There are no tasks here.", nil, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isValidTaskJSON(c.raw); got != c.valid {
				t.Fatalf("isValidTaskJSON = %v, want %v", got, c.valid)
			}
			got := parseProposedTasks(c.raw)
			if len(got) != len(c.want) {
				t.Fatalf("got %d tasks %+v, want %v", len(got), got, c.want)
			}
			for i := range got {
				if got[i].Title != c.want[i] {
					t.Fatalf("task %d = %q, want %q", i, got[i].Title, c.want[i])
				}
			}
		})
	}
}

func TestTaskExtractionPromptAsksForAnObject(t *testing.T) {
	if !strings.Contains(taskExtractionPrompt, `{"tasks": [`) {
		t.Fatal("JSON mode forbids a top-level array; the prompt must ask for {\"tasks\": [...]}")
	}
}

func TestExtractionCalendar(t *testing.T) {
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC) // a Tuesday
	got := extractionCalendar(now)
	for _, want := range []string{"Today is Tuesday 2026-09-29", "Thu 2026-10-01", "Fri 2026-10-02", "Tue 2026-10-06"} {
		if !strings.Contains(got, want) {
			t.Fatalf("calendar %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "Wed 2026-10-07") {
		t.Fatalf("calendar runs past seven days: %q", got)
	}
}

// Ask AI's tools set due dates, so its prompt carries the calendar; a plain
// answer does not need one.
func TestAskAIPromptCarriesTheCalendarWithTools(t *testing.T) {
	got := askAIDatePrompt(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	for _, want := range []string{"Today is Saturday 2026-10-03", "Fri 2026-10-09", "Fri 2026-10-16"} {
		if !strings.Contains(got, want) {
			t.Errorf("date prompt lacks %q: %s", want, got)
		}
	}
	if strings.Contains(AskAISystemPrompt(false), "The coming days:") {
		t.Error("a prompt without tools needs no calendar")
	}
}
