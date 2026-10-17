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
