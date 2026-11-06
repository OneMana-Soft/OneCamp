package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/google/uuid"
)

// The boards a retention rule removes, as the graph is asked for them (miro-44
// head R): in the trash (a live board's deletion time is 0001-01-01 or the
// epoch, so never has() or a nil check) before the cutoff, and no held person
// made or edits them. Dgraph refuses a variable it never uses, so with nobody
// held there are no hold variables at all.
func TestTrashedBoardsFilter(t *testing.T) {
	cutoff := time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)
	vars, filter := trashedBoardsFilter(cutoff, nil)
	if vars != "" {
		t.Errorf("nobody held: vars = %q, want none", vars)
	}
	for _, want := range []string{"type(Board)", dgraphquery.Deleted("board_deleted_at"), `lt(board_deleted_at, "2026-10-04T09:30:00Z")`} {
		if !strings.Contains(filter, want) {
			t.Errorf("filter %q lacks %q", filter, want)
		}
	}
	if strings.Contains(filter, "has(") || strings.Contains(filter, "uid(held") {
		t.Errorf("filter %q: never has(), and no hold variables with nobody held", filter)
	}

	a, b := uuid.New(), uuid.New()
	vars, filter = trashedBoardsFilter(cutoff, []uuid.UUID{a, b, uuid.Nil})
	if !strings.Contains(vars, "eq(user_uuid, ["+dgraphquery.UUIDListOf([]uuid.UUID{a, b})+"])") ||
		!strings.Contains(vars, "heldMade as ~board_created_by") || !strings.Contains(vars, "heldEdits as ~board_editing_users") {
		t.Errorf("held vars = %q; want the held people's boards made and edited", vars)
	}
	if !strings.Contains(filter, "NOT uid(heldMade)") || !strings.Contains(filter, "NOT uid(heldEdits)") {
		t.Errorf("filter %q keeps held people's boards in", filter)
	}
}

// A hold's boards are the boards its people made or edit, deleted ones
// included, matching the words in the title, in the board's id order after a
// part's cursor.
func TestHeldBoardsQuery(t *testing.T) {
	p := uuid.New()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := heldBoardsQuery([]uuid.UUID{p}, HeldBoardQuery{Text: `plan\.x`, From: from, After: "22222222-2222-2222-2222-222222222222"}, 101, true)
	for _, want := range []string{
		`eq(user_uuid, ["` + p.String() + `"])`, "made as ~board_created_by", "edits as ~board_editing_users",
		"uid(made, edits), orderasc: board_uuid, first: 101", `regexp(board_title, /.*plan\.x.*/i)`,
		`ge(board_updated_at, "2026-01-01T00:00:00Z")`, `gt(board_uuid, "22222222-2222-2222-2222-222222222222")`, "board_state_key",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("held boards query lacks %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "board_deleted_at, \"1970") {
		t.Errorf("a hold's boards include deleted ones; the query filters on deletion:\n%s", q)
	}
	if lite := heldBoardsQuery([]uuid.UUID{p}, HeldBoardQuery{}, 50, false); strings.Contains(lite, "board_state") || strings.Contains(lite, "regexp(") {
		t.Errorf("a search with no words reads no drawings and matches every title:\n%s", lite)
	}
}
