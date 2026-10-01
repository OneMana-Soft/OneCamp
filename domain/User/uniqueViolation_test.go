package domain

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lib/pq"
)

// users carries TWO unique columns and they mean opposite things to a caller. A username
// collision is ours to resolve, which is why CreateUserWithMethod retries with a suffix. An email
// collision is not — that address belongs to an account already. Confusing them either retries
// something unretryable or refuses something recoverable, so the two predicates are pinned apart.
func TestUniqueViolationPredicatesDistinguishEmailFromUsername(t *testing.T) {
	// Constraint names verified against a real Postgres by
	// scripts/verify-user-compensation-sql.sh, which asserts the default "<table>_<column>_key"
	// naming this matching relies on.
	emailViolation := &pq.Error{
		Code:       "23505",
		Message:    `duplicate key value violates unique constraint "users_email_id_key"`,
		Constraint: "users_email_id_key",
		Table:      "users",
	}
	usernameViolation := &pq.Error{
		Code:       "23505",
		Message:    `duplicate key value violates unique constraint "users_username_key"`,
		Constraint: "users_username_key",
		Table:      "users",
	}

	cases := []struct {
		name         string
		err          error
		wantEmail    bool
		wantUsername bool
	}{
		{"an email collision", emailViolation, true, false},
		{"a username collision", usernameViolation, false, true},
		{"a foreign key violation is neither", &pq.Error{Code: "23503"}, false, false},
		{"an undefined table is neither", &pq.Error{Code: "42P01"}, false, false},
		{"a plain error is neither", errors.New("boom"), false, false},
		{"nil is neither", nil, false, false},
	}

	for _, c := range cases {
		if got := IsUniqueViolationOnEmail(c.err); got != c.wantEmail {
			t.Errorf("%s: IsUniqueViolationOnEmail = %v, want %v", c.name, got, c.wantEmail)
		}
		if got := IsUniqueViolationOnUsername(c.err); got != c.wantUsername {
			t.Errorf("%s: IsUniqueViolationOnUsername = %v, want %v", c.name, got, c.wantUsername)
		}
	}
}

// THE REGRESSION THIS REPLACES. The previous implementation read the error's TEXT:
//
//	strings.Contains(msg, "duplicate key value") && strings.Contains(msg, "username")
//
// which held only while Postgres phrased the message that way, in English, with the constraint
// still called users_username_key. Here is a genuine username violation whose message does not
// contain either phrase — a renamed constraint, or a non-English locale. The old check returned
// false, which does not fail loudly: it means the username-collision retry loop in
// CreateUserWithMethod simply stops retrying, and SSO provisioning starts failing for exactly the
// directory collisions that loop exists to absorb.
func TestUsernameViolationIsRecognisedWithoutRelyingOnMessageText(t *testing.T) {
	localised := &pq.Error{
		Code:       "23505",
		Message:    "clé dupliquée viole la contrainte unique", // no "duplicate key value"
		Constraint: "users_username_key",
		Table:      "users",
	}
	if !IsUniqueViolationOnUsername(localised) {
		t.Error("a username violation went unrecognised because its message was not the expected " +
			"English phrasing — the retry loop would silently stop retrying")
	}

	// The column field alone is enough when the constraint name is absent.
	byColumn := &pq.Error{Code: "23505", Column: "username", Table: "users"}
	if !IsUniqueViolationOnUsername(byColumn) {
		t.Error("a violation identified only by Column was not recognised")
	}
}

// The old check also matched too eagerly: anything whose message merely mentioned the word would
// qualify. A failure that is not a unique violation must never be reported as one, or a caller
// retries a write that will never succeed.
func TestNonUniqueViolationMentioningTheColumnIsNotMatched(t *testing.T) {
	notAConflict := &pq.Error{
		Code:    "23502", // not_null_violation
		Message: `null value in column "username" violates not-null constraint`,
		Column:  "username",
		Table:   "users",
	}
	if IsUniqueViolationOnUsername(notAConflict) {
		t.Error("a NOT NULL violation on username was reported as a uniqueness conflict; the " +
			"retry loop would append suffixes forever against an error suffixes cannot fix")
	}
}

// Errors reach these predicates through several layers, some of which wrap. A type assertion
// instead of errors.As would report false for a wrapped driver error, which is the failure mode
// where a duplicate email surfaces as an unexplained 500.
func TestPredicatesSeeThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("domain/CreateUserWithMethod: %w",
		&pq.Error{Code: "23505", Constraint: "users_email_id_key"})

	if !IsUniqueViolationOnEmail(wrapped) {
		t.Error("a wrapped email violation was not recognised")
	}
	if IsUniqueViolationOnUsername(wrapped) {
		t.Error("an email violation was misreported as a username violation")
	}
}
