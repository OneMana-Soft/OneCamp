package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildPermissionFilterBoardComment verifies the comment permission branch
// grants board comments via the board's role lists / public flag (parity with
// doc/post/task/chat comments), and that the filter remains valid JSON.
func TestBuildPermissionFilterBoardComment(t *testing.T) {
	filter := buildPermissionFilter("user-123", []string{"ch-1"}, []string{"proj-1"}, []string{"grp-1"})

	// Must be valid JSON (a malformed clause would break SearchSimilar).
	var parsed map[string]any
	if err := json.Unmarshal([]byte(filter), &parsed); err != nil {
		t.Fatalf("permission filter is not valid JSON: %v\n%s", err, filter)
	}

	// Board comment permission terms must be present so board comments are
	// retrievable by their owner / role members / on a public board.
	for _, want := range []string{
		"board_created_by_user_id",
		"board_reading_users",
		"board_editing_users",
		"board_commenting_users",
		"board_public",
	} {
		if !strings.Contains(filter, want) {
			t.Errorf("permission filter missing board term %q", want)
		}
	}

	// The user's id must be interpolated into the board membership terms.
	if !strings.Contains(filter, `"board_reading_users": "user-123"`) {
		t.Errorf("board_reading_users term not scoped to the user:\n%s", filter)
	}

	// board_public is a public-only grant (true), never a blanket false that
	// would leak non-board comments.
	if strings.Contains(filter, `"board_public": false`) {
		t.Errorf("board_public must only match true (public boards), got a false term:\n%s", filter)
	}
}

// TestBuildPermissionFilterCommentDocGrantConstrained guards against the
// blanket-match leak: doc_private is stored false on every embedding (no
// omitempty), so the comment clause's public-doc grant must be gated on the
// presence of doc_uuid, otherwise it matches (and leaks) every comment.
func TestBuildPermissionFilterCommentDocGrantConstrained(t *testing.T) {
	filter := buildPermissionFilter("user-123", nil, nil, nil)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(filter), &parsed); err != nil {
		t.Fatalf("permission filter is not valid JSON: %v\n%s", err, filter)
	}

	// The public-doc grant in the comment branch must require doc_uuid to exist,
	// so it cannot match post/chat/task comments (which never set doc_uuid).
	wantConstrained := `{"bool": {"must": [{"exists": {"field": "doc_uuid"}}, {"term": {"doc_private": false}}]}}`
	if !strings.Contains(filter, wantConstrained) {
		t.Errorf("comment clause must gate the public-doc grant on doc_uuid existence; not found:\n%s", filter)
	}
}
