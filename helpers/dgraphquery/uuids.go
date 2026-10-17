package dgraphquery

import (
	"strings"

	"github.com/google/uuid"
)

// UUIDList writes ids for a DQL list, as in eq(doc_uuid, [<list>]): each one
// that is a uuid, once, quoted and written as uuids are (lower case), so
// nothing from a request reaches the query text unchecked. "" when none is.
func UUIDList(ids []string) string {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil && !seen[u] {
			seen[u] = true
			out = append(out, `"`+u.String()+`"`)
		}
	}
	return strings.Join(out, ", ")
}
