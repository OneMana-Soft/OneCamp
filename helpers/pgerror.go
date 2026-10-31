package helpers

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
)

// Postgres condition codes worth recognising by name rather than by string matching.
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const pgUniqueViolation = "23505"

// IsUniqueViolation reports whether err is Postgres refusing a duplicate value.
//
// WHY A PREDICATE, and not a string match on the message. A unique violation is the one database
// error a handler genuinely needs to distinguish, because it is the only one that is the USER's
// business rather than the server's: they picked a name somebody already has, and telling them
// so is the difference between "A channel named general already exists" and "something went
// wrong". Matching on the message text would break with a Postgres locale or wording change, and
// worse, encourages putting the driver's own message in front of the user.
//
// DO NOT put the error itself in an HTTP response. A *pq.Error has exported fields, so encoding
// it hands the client the constraint name, the table and column, and Postgres' own source file
// and line — schema detail nobody outside the server needs. Use the predicate to choose a
// message you wrote.
func IsUniqueViolation(err error) bool { return pgErrorCodeIs(err, pgUniqueViolation) }

// IsQueryCanceled is whether Postgres stopped a statement: its statement_timeout, or a cancel.
func IsQueryCanceled(err error) bool { return pgErrorCodeIs(err, "57014") }

// IsUniqueViolationOn reports whether err is Postgres refusing a duplicate
// value for the one unique constraint named (booking_pages_slug_key), so a
// handler names the clash it knows ("another page uses that address") and
// not one a different constraint on the same row would make.
func IsUniqueViolationOn(err error, constraint string) bool {
	return IsUniqueViolation(err) && pgConstraint(err) == constraint
}

// There is deliberately no IsForeignKeyViolation or IsNotNullViolation here. Both were written
// alongside IsUniqueViolation because a set of three looked more complete than one, and neither
// ever acquired a caller — a foreign-key or NOT NULL failure is a server-side bug, not something
// a handler should catch and translate for a user, so nothing needed to distinguish them. They
// were removed rather than kept "for later": each had a passing test, which is what made them
// look used, and an untouched predicate is a claim about behaviour nobody is checking against
// reality. Add one back at the point a caller exists.

// IsRowRejected reports whether Postgres refused a row for what it holds: a value it can't
// store (class 22, data exception, such as invalid UTF-8) or a rule the row breaks (class 23,
// integrity constraint violation, such as a foreign key to a person since deleted).
//
// For a writer that sends several rows in one transaction and has to tell "this row can't be
// written" from "nothing can be written just now" (a lost connection, a timeout, a database
// shutting down): the first is the row's alone and the others can go without it, the second is
// everyone's (models/AdminAudit chainWriter.go).
func IsRowRejected(err error) bool {
	code := pgErrorCode(err)
	return len(code) == 5 && (code[:2] == "22" || code[:2] == "23")
}

// pgErrorCodeIs unwraps err looking for a driver error carrying code.
func pgErrorCodeIs(err error, code string) bool {
	return err != nil && pgErrorCode(err) == code
}

// pgConstraint is the constraint the driver error err carries names, or "" for none.
func pgConstraint(err error) string {
	var pgxErr *pgconn.PgError
	if errors.As(err, &pgxErr) {
		return pgxErr.ConstraintName
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Constraint
	}
	return ""
}

// pgErrorCode is the condition code of the driver error err carries, or "" for none.
//
// errors.As, not a type assertion: errors reach handlers through several layers here and some of
// them wrap. A plain assertion would silently report false for a wrapped driver error, which is
// the failure mode where a duplicate name shows up as a 500.
func pgErrorCode(err error) string {
	if err == nil {
		return ""
	}
	// The server talks to Postgres through pgx; lib/pq errors still come from
	// the tools that use it. Checking only pq's type made every duplicate look
	// like a server error ("something went wrong" for a taken channel name).
	var pgxErr *pgconn.PgError
	if errors.As(err, &pgxErr) {
		return pgxErr.Code
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return string(pqErr.Code)
	}
	return ""
}
