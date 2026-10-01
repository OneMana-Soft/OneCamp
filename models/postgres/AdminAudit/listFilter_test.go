package models

import (
	"strings"
	"testing"
)

// The filter's whole correctness is placeholder numbering: a clause that names
// $1 for a value that landed at $2 is a query that runs, returns rows, and
// returns the wrong ones. These pin the composition.

func TestNoFilterIsNoWhere(t *testing.T) {
	where, args := ListFilter{}.whereClause()
	if where != "" || len(args) != 0 {
		t.Errorf("an empty filter produced %q with %d args", where, len(args))
	}
}

func TestEachFilterNumbersItsOwnPlaceholder(t *testing.T) {
	where, args := ListFilter{Category: "agent", Initiators: []string{"schedule", "event"}}.whereClause()
	if len(args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(args))
	}
	if !strings.Contains(where, "category = $1") {
		t.Errorf("category is not $1: %q", where)
	}
	if !strings.Contains(where, "= ANY($2)") {
		t.Errorf("initiators are not $2: %q", where)
	}
	if !strings.Contains(where, " AND ") {
		t.Errorf("two clauses were not joined: %q", where)
	}
}

func TestInitiatorAloneIsPlaceholderOne(t *testing.T) {
	// The bug this guards: numbering placeholders by position in the struct
	// rather than by how many clauses came before, so a filter with no category
	// asks for $2 with one argument.
	where, args := ListFilter{Initiators: []string{"handoff"}}.whereClause()
	if len(args) != 1 || !strings.Contains(where, "= ANY($1)") {
		t.Errorf("initiator alone should be $1 with one arg, got %q with %d", where, len(args))
	}
}

func TestInitiatorFilterReadsTheMetadataKey(t *testing.T) {
	// The key must be the one the business layer writes. A drift here is a
	// filter that matches nothing and says so with an empty screen.
	where, _ := ListFilter{Initiators: []string{"schedule"}}.whereClause()
	if !strings.Contains(where, "metadata ->> 'initiator'") {
		t.Errorf("filter does not read metadata.initiator: %q", where)
	}
}
