package helpers

import (
	"errors"

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

// There is deliberately no IsForeignKeyViolation or IsNotNullViolation here. Both were written
// alongside IsUniqueViolation because a set of three looked more complete than one, and neither
// ever acquired a caller — a foreign-key or NOT NULL failure is a server-side bug, not something
// a handler should catch and translate for a user, so nothing needed to distinguish them. They
// were removed rather than kept "for later": each had a passing test, which is what made them
// look used, and an untouched predicate is a claim about behaviour nobody is checking against
// reality. Add one back at the point a caller exists.
// pgErrorCodeIs unwraps err looking for a driver error carrying code.
//
// errors.As, not a type assertion: errors reach handlers through several layers here and some of
// them wrap. A plain assertion would silently report false for a wrapped driver error, which is
// the failure mode where a duplicate name shows up as a 500.
func pgErrorCodeIs(err error, code string) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return string(pqErr.Code) == code
	}
	return false
}
