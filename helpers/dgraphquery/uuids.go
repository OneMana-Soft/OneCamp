package dgraphquery

import (
	"strings"

	"github.com/google/uuid"
)

// UUIDList writes ids for a DQL list, as in eq(doc_uuid, [<list>]): each one
// that is a uuid, once, quoted and written as uuids are (lower case), so
// nothing from a request reaches the query text unchecked. "" when none is.
func UUIDList(ids []string) string {
	return strings.Join(QuotedUUIDs(ids), ", ")
}

// QuotedUUIDs is UUIDList's items before they are joined: each id that is a
// uuid, once, quoted, lower case. For a caller that counts or joins them
// itself.
func QuotedUUIDs(ids []string) []string {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil && !seen[u] {
			seen[u] = true
			out = append(out, `"`+u.String()+`"`)
		}
	}
	return out
}

// UUIDListOf is UUIDList for ids already parsed: each once, quoted, with the
// zero uuid left out (it names nothing). "" when none is left.
func UUIDListOf(ids []uuid.UUID) string {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, u := range ids {
		if u != uuid.Nil && !seen[u] {
			seen[u] = true
			out = append(out, `"`+u.String()+`"`)
		}
	}
	return strings.Join(out, ", ")
}
