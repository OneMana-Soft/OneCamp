package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

type fakeLLM struct{ ai.LLMProvider }

// fakedExtraction counts what an extraction asked for: the person's model
// (one of their AI requests spent) and the model's answer.
type fakedExtraction struct{ models, chats int }

// fakeExtraction makes AI on and the model answer with out, as
// ChatJSONWithRetry does: an answer that never reads as task JSON is an error.
// check, when given, sees the prompt and the message. The clock is a Tuesday,
// and Priya is a member.
func fakeExtraction(t *testing.T, out string, check func(system, user string)) *fakedExtraction {
	t.Helper()
	savedOn, savedModel, savedChat, savedNow, savedWho := extractionOn, extractionModel, chatExtraction, extractionNow, assigneeOf
	t.Cleanup(func() {
		extractionOn, extractionModel, chatExtraction, extractionNow, assigneeOf = savedOn, savedModel, savedChat, savedNow, savedWho
	})
	faked := &fakedExtraction{}
	extractionOn = func() bool { return true }
	extractionModel = func(context.Context, string) (ai.LLMProvider, *ai.CircuitBreaker, error) {
		faked.models++
		return fakeLLM{}, ai.NewCircuitBreaker(5, time.Minute), nil
	}
	chatExtraction = func(_ context.Context, _ ai.LLMProvider, _ *ai.CircuitBreaker, system, user string, _ ai.ChatOptions, valid func(string) bool) (string, error) {
		faked.chats++
		if check != nil {
			check(system, user)
		}
		if !valid(out) {
			return "", errors.New("the model's answer never read as JSON")
		}
		return out, nil
	}
	extractionNow = func() time.Time { return time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC) }
	assigneeOf = func(_ context.Context, _, name string, _ bool) (*dgraphStruct.DgraphUser, error) {
		if strings.EqualFold(name, "Priya") {
			return &dgraphStruct.DgraphUser{Uuid: "u-priya", UserName: "Priya Nair"}, nil
		}
		return nil, errors.New("no such member")
	}
	return faked
}

const standup = "Priya will send the Q4 numbers to finance by Friday, and someone should book the room for the offsite."

// extractFromText is "turn discussion into tasks" on pasted text.
func extractFromText(t *testing.T, text string) *ExtractTasksResult {
	t.Helper()
	res, err := ExtractActionItems(context.Background(), member(), "text", "", text)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Each item becomes a proposed task: an item with no title is dropped, the
// owner is resolved to a member (or kept as written), the due date is the end
// of that working day and the priority one of three.
func TestTheModelsItemsBecomeProposedTasks(t *testing.T) {
	fakeExtraction(t, "```json\n"+`[
      {"title": "Draft the pricing doc", "assignee": "Priya", "due": "2026-07-03", "priority": "high", "details": "for the launch"},
      {"title": "  ", "assignee": "Bob"},
      {"title": "Follow up on vendor contract", "assignee": "Alice", "priority": "weird"}
    ]`+"\n```", func(system, user string) {
		if system != taskExtractionPrompt || !strings.Contains(user, "Text:\n"+standup) || !strings.Contains(user, "Fri 2026-10-16") {
			t.Errorf("asked with %q", user)
		}
	})
	res := extractFromText(t, standup)
	if !res.Enabled || len(res.Tasks) != 2 || res.Note != "" {
		t.Fatalf("got %+v", res)
	}
	want := []ProposedTask{
		{Title: "Draft the pricing doc", Description: "for the launch", AssigneeName: "Priya Nair", AssigneeUUID: "u-priya", Due: "2026-07-03T17:00:00Z", Priority: dgraphStruct.TASK_PRIORITY_HIGH},
		{Title: "Follow up on vendor contract", AssigneeName: "Alice", Priority: dgraphStruct.TASK_PRIORITY_MEDIUM},
	}
	for i := range want {
		if res.Tasks[i] != want[i] {
			t.Errorf("task %d: got %+v, want %+v", i, res.Tasks[i], want[i])
		}
	}
}

// A title is cleaned and bounded the same way whatever it came from (batch 16
// audit: the two flows had drifted): one line, no control characters, at most
// extractMaxTitleLen characters and never a character cut in half. A board
// label once cut a title at 120 bytes, mid-character.
func TestATitleIsCleanedTheSameWayFromEverySource(t *testing.T) {
	title := "aa\t" + strings.Repeat("€", 200) + "\u0007\nend"
	out := `{"tasks":[{"title":` + quote(title) + `,"details":` + quote(strings.Repeat("é ", 300)) + `,"assignee":" Priya ","block":3}]}`

	fakeExtraction(t, out, nil)
	fromText := extractFromText(t, standup).Tasks
	fakeTasksDoc(t, meetingNotes, out)
	fromDoc, err := ExtractDocActionItems(context.Background(), member(), recDoc)
	if err != nil || len(fromText) != 1 || len(fromDoc.Tasks) != 1 {
		t.Fatalf("text %+v, doc %+v %v", fromText, fromDoc, err)
	}
	got := fromText[0]
	if fromDoc.Tasks[0].ProposedTask != got {
		t.Errorf("the sources disagree:\n text %+v\n doc  %+v", got, fromDoc.Tasks[0].ProposedTask)
	}
	if n := utf8.RuneCountInString(got.Title); n != extractMaxTitleLen || !utf8.ValidString(got.Title) || got.Title != "aa "+strings.Repeat("€", extractMaxTitleLen-3) {
		t.Errorf("title of %d characters: %q", n, got.Title)
	}
	if n := utf8.RuneCountInString(got.Description); n > extractMaxDescLen || n < extractMaxDescLen-1 || strings.HasSuffix(got.Description, " ") {
		t.Errorf("details of %d characters: %q", n, got.Description)
	}
	if got.AssigneeUUID != "u-priya" {
		t.Errorf("owner: %+v", got)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// No more than extractMaxTasks tasks come back, whatever the model returns.
func TestNoMoreThanTwentyTasksAreProposed(t *testing.T) {
	items := make([]string, extractMaxTasks+5)
	for i := range items {
		items[i] = `{"title":"t"}`
	}
	fakeExtraction(t, `{"tasks":[`+strings.Join(items, ",")+`]}`, nil)
	if got := extractFromText(t, standup); len(got.Tasks) != extractMaxTasks {
		t.Errorf("got %d tasks", len(got.Tasks))
	}
}

// Too little to read is said without asking for the person's model, so it
// spends none of their AI requests.
func TestTooLittleToReadAsksNoModel(t *testing.T) {
	faked := fakeExtraction(t, `{"tasks":[]}`, nil)
	if res := extractFromText(t, "  Sounds good.  "); !res.Enabled || res.Note != "There isn't enough here to pull action items from." || faked.models != 0 || faked.chats != 0 {
		t.Errorf("got %+v, model asked for %d times", res, faked.models)
	}
}

// A model that never answers in JSON gives a note, not an error; nothing found
// says so; AI off hides the surface and asks nothing.
func TestWhatTheExtractionSaysWhenThereAreNoTasks(t *testing.T) {
	fakeExtraction(t, "There are no tasks here.", nil)
	if res := extractFromText(t, standup); !res.Enabled || len(res.Tasks) != 0 || res.Note != "Couldn't read the action items just now. Try again in a moment." {
		t.Errorf("a failed model: %+v", res)
	}
	fakeExtraction(t, `{"tasks":[]}`, nil)
	if res := extractFromText(t, standup); res.Note != "No clear action items found." {
		t.Errorf("nothing found: %+v", res)
	}
	faked := fakeExtraction(t, `{"tasks":[]}`, nil)
	extractionOn = func() bool { return false }
	if res := extractFromText(t, standup); res.Enabled || res.Note != "" || faked.models != 0 {
		t.Errorf("AI off: %+v", res)
	}
}

// A doc asked for through "turn discussion into tasks" is read as a doc: its
// numbered lines, with the doc's own prompt.
func TestADocIsReadAsADocWhereverItIsAskedFor(t *testing.T) {
	fakeTasksDoc(t, meetingNotes, `{"tasks":[{"title":"Send the deck","assignee":"Priya","block":3}]}`)
	res, err := ExtractActionItems(context.Background(), member(), "doc", recDoc, "")
	if err != nil || len(res.Tasks) != 1 || res.Tasks[0].AssigneeUUID != "u-priya" || res.ScannedCount != 4 {
		t.Errorf("got %+v %v", res, err)
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
func TestTheModelsAnswerIsReadInEveryShape(t *testing.T) {
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
		"a doc's line":  {`{"title": "Send the deck", "block": "[3]"}`, []string{"Send the deck"}, true},
		"other object":  {`{"answer": "none"}`, nil, false},
		"prose":         {"There are no tasks here.", nil, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isValidTaskJSON(c.raw); got != c.valid {
				t.Fatalf("isValidTaskJSON = %v, want %v", got, c.valid)
			}
			got, _ := decodeTaskItems(c.raw)
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
