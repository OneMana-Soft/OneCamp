package helpers

import (
	"fmt"
	"strings"
)

// NamedLen is one slice's name and length, for reporting which parallel input disagreed.
type NamedLen struct {
	Name string
	Len  int
}

// RequireSameLength checks that slices meant to be read at the same index really are the same
// length, and names the offenders when they are not.
//
// Why this exists rather than a bare len(a) != len(b) at each site. Bulk writers here take
// several slices and pair them positionally — row i of one is row i of another. When those
// slices are built by a caller under different conditions, they can drift apart, and the
// failure is ugly in both directions: a short slice panics with an index out of range from
// deep inside a database helper, and a long one silently drops rows with no error at all.
//
// This is not hypothetical drift. Forwarding a message to several destinations accumulated
// exactly this kind of parallel state, and the lists grew under different conditions, which
// delivered a group chat's message into an unrelated DM. The lists feeding these writers are
// aligned today; the point is that the invariant is now stated and enforced where it is
// relied upon, instead of being a property of code somewhere else.
//
// what names the operation for the error message. Fewer than two fields is a programming
// error in the caller, not a data problem, and says so.
func RequireSameLength(what string, fields ...NamedLen) error {
	if len(fields) < 2 {
		return fmt.Errorf("%s: RequireSameLength needs at least two fields to compare, got %d",
			what, len(fields))
	}

	want := fields[0]
	for _, f := range fields[1:] {
		if f.Len == want.Len {
			continue
		}
		parts := make([]string, 0, len(fields))
		for _, g := range fields {
			parts = append(parts, fmt.Sprintf("%s=%d", g.Name, g.Len))
		}
		return fmt.Errorf("%s: parallel inputs must be the same length, got %s",
			what, strings.Join(parts, " "))
	}
	return nil
}
