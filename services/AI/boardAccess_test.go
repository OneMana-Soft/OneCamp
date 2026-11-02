package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// An AI index that took the board fields by itself mapped them as text, so a
// term for a whole uuid matched only their keyword sub-field, and a private
// board's comments never reached its members. Each grant asks both forms.
func TestBoardGrantAsksEachFieldAndItsKeyword(t *testing.T) {
	got := strings.Join(boardGrant("u-1"), "\n")
	for _, f := range boardAccessFields {
		for _, field := range []string{f, f + ".keyword"} {
			if !strings.Contains(got, `{"term": {"`+field+`": "u-1"}}`) {
				t.Errorf("no grant on %s:\n%s", field, got)
			}
		}
	}
	if !strings.Contains(got, `{"term": {"board_public": true}}`) {
		t.Errorf("a public board's entries aren't granted:\n%s", got)
	}
}

// A board's own entry is granted only as a board, never by another clause.
func TestTheFilterGrantsBoardsEntriesAsTheBoard(t *testing.T) {
	filter := buildPermissionFilter("u-1", nil, nil, nil)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(filter), &parsed); err != nil {
		t.Fatalf("the filter isn't JSON: %v\n%s", err, filter)
	}
	want := boardClause("u-1")
	if !strings.Contains(filter, want) {
		t.Fatalf("no board clause in the filter:\n%s", filter)
	}
	if !strings.Contains(want, `{"term": {"content_type": "board"}}`) || !strings.Contains(want, `"board_reading_users.keyword": "u-1"`) {
		t.Errorf("the board clause doesn't grant as the board: %s", want)
	}
}

func TestBoardAccessUpdateBodyAsksTheBoardInBothForms(t *testing.T) {
	body, err := boardAccessUpdateBody("b-1", BoardAccess{Public: false, CreatedBy: "ana", Reading: []string{"ben"}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Script struct {
			Params struct {
				Access map[string]any `json:"access"`
			} `json:"params"`
		} `json:"script"`
		Query struct {
			Bool struct {
				Should []map[string]map[string]string `json:"should"`
			} `json:"bool"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	a := got.Script.Params.Access
	if a["public"] != false || a["created_by"] != "ana" || len(a["reading"].([]any)) != 1 || a["editing"] == nil || a["commenting"] == nil {
		t.Errorf("access params: %v", a)
	}
	s := got.Query.Bool.Should
	if len(s) != 2 || s[0]["term"]["board_uuid"] != "b-1" || s[1]["term"]["board_uuid.keyword"] != "b-1" {
		t.Errorf("the board isn't asked in both forms: %v", s)
	}
}

func TestBoardAccessIsWrittenOnTheEntry(t *testing.T) {
	var d EmbeddingDoc
	BoardAccess{Public: true, CreatedBy: "ana", Editing: []string{"ben"}}.on(&d, "b-1")
	if d.BoardUUID != "b-1" || !d.BoardPublic || d.BoardCreatedByUserID != "ana" || len(d.BoardEditingUsers) != 1 {
		t.Errorf("entry: %+v", d)
	}
}
