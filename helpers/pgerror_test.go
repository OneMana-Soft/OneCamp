package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lib/pq"
)

func uniqueViolation() *pq.Error {
	return &pq.Error{
		Severity:   "ERROR",
		Code:       "23505",
		Message:    `duplicate key value violates unique constraint "channels_ch_name_key"`,
		Detail:     `Key (ch_name)=(general) already exists.`,
		Table:      "channels",
		Column:     "ch_name",
		Constraint: "channels_ch_name_key",
		File:       "nbtinsert.c",
		Line:       "666",
		Routine:    "_bt_check_unique",
	}
}

// The predicate must recognise a unique violation and must NOT claim any other driver condition
// is one. The neighbouring codes are kept in the table precisely because they must come back
// false: a foreign-key or NOT NULL failure is a server-side bug, and reporting it to a user as
// "that name is taken" would be worse than a 500.
func TestPostgresConditionPredicates(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		unique bool
	}{
		{"unique violation", uniqueViolation(), true},
		{"foreign key violation", &pq.Error{Code: "23503"}, false},
		{"not null violation", &pq.Error{Code: "23502"}, false},
		{"some other driver error", &pq.Error{Code: "42P01"}, false},
		{"a plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}

	for _, c := range cases {
		if got := IsUniqueViolation(c.err); got != c.unique {
			t.Errorf("%s: IsUniqueViolation = %v, want %v", c.name, got, c.unique)
		}
	}
}

// Errors travel through several layers here and some of them wrap. A type assertion instead of
// errors.As would report false for a wrapped driver error — which is the failure mode where a
// duplicate name surfaces to the user as a 500 instead of "that name is taken".
func TestPredicatesSeeThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("domain/CreateChannel: %w", uniqueViolation())
	if !IsUniqueViolation(wrapped) {
		t.Error("a wrapped unique violation was not recognised")
	}

	twice := fmt.Errorf("business: %w", wrapped)
	if !IsUniqueViolation(twice) {
		t.Error("a doubly-wrapped unique violation was not recognised")
	}
}

// The reason the predicate exists: so nobody needs to put the driver error in a response.
//
// This test documents what would happen if they did. It is not testing our code — it is pinning
// the fact that motivates the rule, so that a future reader who is tempted to add
// `"err": err` to an envelope can see exactly what the client receives.
func TestDriverErrorMustNeverBeSerialisedToAClient(t *testing.T) {
	body, err := json.Marshal(map[string]any{"err": uniqueViolation()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(body)

	// Every one of these is server-side detail that a client has no business seeing.
	for _, leaked := range []string{
		"channels_ch_name_key", // the constraint name
		"channels",             // the table
		"ch_name",              // the column
		"nbtinsert.c",          // Postgres' own source file
		"_bt_check_unique",     // and its internal routine
	} {
		if !strings.Contains(got, leaked) {
			t.Errorf("expected the marshalled driver error to expose %q — if this no longer "+
				"holds, the warning in pgerror.go should be revisited", leaked)
		}
	}
}
