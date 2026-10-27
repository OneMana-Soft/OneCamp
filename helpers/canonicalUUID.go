package helpers

import (
	"strings"

	"github.com/google/uuid"
)

// IsCanonicalUUID says whether s is a uuid written as uuids are written: 36
// characters, hyphenated, nothing around it (either case). An id that would
// parse in another form (braces, a urn: prefix, no hyphens) is refused, so one
// id can't name a thing several ways.
func IsCanonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == strings.ToLower(s)
}

// AllCanonicalUUIDs says whether every id in the lists is a canonical uuid
// ("" names no one and is let through), as a request naming people by their
// uuids must be before it is acted on.
func AllCanonicalUUIDs(lists ...[]string) bool {
	for _, ids := range lists {
		for _, id := range ids {
			if id != "" && !IsCanonicalUUID(id) {
				return false
			}
		}
	}
	return true
}

// UUIDStrings is ids as text, in their order, each as uuids are written. No
// ids is an empty list, never nil: pq.Array binds nil as NULL, and nothing
// compares true or false with NULL.
func UUIDStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
