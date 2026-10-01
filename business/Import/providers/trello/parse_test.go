package trello

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// fixtureBoard is a minimal-but-realistic Trello board JSON. Field
// names match Trello's real export so the parser exercises the same
// codepath as a live API response.
const fixtureBoard = `{
  "id": "65a4d680cafef00d0badf00d",
  "name": "Sprint 23 Backlog",
  "desc": "Q1 2024 work",
  "url": "https://trello.com/b/abc/sprint-23",
  "members": [
    {"id":"u1","username":"alice","fullName":"Alice Adams","avatarUrl":"https://trello.com/u1"},
    {"id":"u2","username":"bob","fullName":"Bob Brown","avatarUrl":""}
  ],
  "memberships": [
    {"id":"m1","idMember":"u1","memberType":"admin"},
    {"id":"m2","idMember":"u2","memberType":"normal"}
  ],
  "labels": [
    {"id":"l1","name":"P1","color":"red"},
    {"id":"l2","name":"bug","color":"yellow"}
  ],
  "lists": [
    {"id":"list1","name":"Backlog","closed":false},
    {"id":"list2","name":"In Progress","closed":false},
    {"id":"list3","name":"Done","closed":false}
  ],
  "cards": [
    {
      "id":"65a4d680ffffffff00000001",
      "idShort":1,
      "idList":"list2",
      "idBoard":"65a4d680cafef00d0badf00d",
      "name":"Implement login",
      "desc":"Use OAuth.\n\nSee #5.",
      "url":"https://trello.com/c/aaa",
      "closed":false,
      "due":"2024-02-01T00:00:00.000Z",
      "start":"2024-01-15T00:00:00.000Z",
      "dueComplete":false,
      "dateLastActivity":"2024-01-20T10:00:00.000Z",
      "idMembers":["u1"],
      "labels":[{"id":"l1","name":"P1"}],
      "checklists":[
        {"id":"cl1","name":"Steps","checkItems":[
          {"id":"ci1","name":"Write spec","state":"complete"},
          {"id":"ci2","name":"Implement","state":"incomplete"}
        ]}
      ],
      "attachments":[],
      "badges":{"comments":2,"attachments":0}
    },
    {
      "id":"65a4d680ffffffff00000002",
      "idShort":2,
      "idList":"list3",
      "idBoard":"65a4d680cafef00d0badf00d",
      "name":"Set up CI",
      "desc":"",
      "url":"https://trello.com/c/bbb",
      "closed":false,
      "dueComplete":true,
      "idMembers":[],
      "labels":[],
      "checklists":[],
      "attachments":[],
      "badges":{"comments":0,"attachments":0}
    },
    {
      "id":"65a4d680ffffffff00000003",
      "idShort":3,
      "idList":"list1",
      "idBoard":"65a4d680cafef00d0badf00d",
      "name":"Old card",
      "desc":"",
      "url":"https://trello.com/c/ccc",
      "closed":true,
      "idMembers":[],
      "labels":[],
      "checklists":[],
      "attachments":[],
      "badges":{"comments":0,"attachments":0}
    }
  ],
  "actions":[
    {"id":"a1","type":"commentCard","date":"2024-01-18T09:00:00.000Z",
     "idMemberCreator":"u1",
     "data":{"text":"LGTM","card":{"id":"65a4d680ffffffff00000001"}}},
    {"id":"a2","type":"commentCard","date":"2024-01-19T11:00:00.000Z",
     "idMemberCreator":"u2",
     "data":{"text":"thanks","card":{"id":"65a4d680ffffffff00000001"}}}
  ]
}`

// TestParseFixtureBoard confirms a known board JSON parses into the
// shape every Iter* expects. We don't go through resolveBoard because
// that requires a job + token; we exercise the unmarshal directly.
func TestParseFixtureBoard(t *testing.T) {
	var b trelloBoard
	if err := json.Unmarshal([]byte(fixtureBoard), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.ID != "65a4d680cafef00d0badf00d" {
		t.Fatalf("board id: %q", b.ID)
	}
	if len(b.Members) != 2 {
		t.Fatalf("members: %d", len(b.Members))
	}
	if len(b.Cards) != 3 {
		t.Fatalf("cards: %d", len(b.Cards))
	}
	if len(b.Actions) != 2 {
		t.Fatalf("actions: %d", len(b.Actions))
	}
	// Closed cards must be flagged so the orchestrator's status mapping
	// (defaultStatusMap "archived"->"canceled") fires.
	closedCount := 0
	for _, c := range b.Cards {
		if c.Closed {
			closedCount++
		}
	}
	if closedCount != 1 {
		t.Fatalf("expected 1 closed card, got %d", closedCount)
	}
}

// TestCollectListNames mirrors what Plan emits to the operator's
// status-mapping UI.
func TestCollectListNames(t *testing.T) {
	var b trelloBoard
	if err := json.Unmarshal([]byte(fixtureBoard), &b); err != nil {
		t.Fatal(err)
	}
	names := collectListNames(&b)
	// Lists in order + "archived" appended because there's a closed card.
	want := []string{"Backlog", "In Progress", "Done", "archived"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("[%d] got %q, want %q", i, n, want[i])
		}
	}
}

// TestStatusMappingOnFixture confirms the default status map clamps
// fixture list names to OneCamp statuses correctly. This is the actual
// path the orchestrator uses on every imported task.
func TestStatusMappingOnFixture(t *testing.T) {
	prov := New()
	defaults := prov.DefaultStatusMap()
	cases := map[string]string{
		"backlog":     "backlog",
		"in progress": "inProgress",
		"done":        "done",
		"archived":    "canceled",
	}
	for src, want := range cases {
		got := importProvider.ApplyStatusMap(src, nil, defaults)
		if got != want {
			t.Errorf("status %q: got %q, want %q", src, got, want)
		}
	}
}

// TestPriorityMappingOnFixture confirms the P1 label maps to "high".
func TestPriorityMappingOnFixture(t *testing.T) {
	prov := New()
	defaults := prov.DefaultPriorityMap()
	if got := importProvider.ApplyPriorityMap("p1", nil, defaults); got != "high" {
		t.Fatalf("p1: got %q, want high", got)
	}
}

// TestCommentParsingFromActions confirms commentCard actions extract
// cleanly from board.Actions for a specific card id. Mirrors the
// IterCommentsOfTask board_json branch.
func TestCommentParsingFromActions(t *testing.T) {
	var b trelloBoard
	if err := json.Unmarshal([]byte(fixtureBoard), &b); err != nil {
		t.Fatal(err)
	}
	const cardID = "65a4d680ffffffff00000001"
	got := []trelloAction{}
	for _, a := range b.Actions {
		if a.Type == "commentCard" && a.Data.Card.ID == cardID {
			got = append(got, a)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(got))
	}
	if !strings.Contains(got[0].Data.Text, "LGTM") {
		t.Fatalf("first comment body: %q", got[0].Data.Text)
	}
}

// TestParseTimezones makes sure parseTrelloTime handles both the
// canonical "Z" suffix and the millisecond suffix Trello uses.
func TestParseTimezones(t *testing.T) {
	cases := []string{
		"2024-02-01T00:00:00.000Z",
		"2024-01-20T10:00:00.000Z",
	}
	for _, s := range cases {
		got := parseTrelloTime(s)
		if got == nil || got.IsZero() {
			t.Errorf("parseTrelloTime(%q) returned nil/zero", s)
		}
	}
	if parseTrelloTime("") != nil {
		t.Errorf("empty string should yield nil")
	}
	if parseTrelloTime("not-a-date") != nil {
		t.Errorf("invalid string should yield nil")
	}
}

// dummyContext satisfies the ctx parameter where we don't actually
// dispatch async work. Kept private to this test package.
func dummyContext() context.Context { return context.Background() }

var _ = dummyContext
var _ = time.Now
