package liveness

import (
	"strings"
	"testing"
)

// A doc's uuid is asked for as uuids are written (lower case), once: an
// upper-case id from a caller was quoted as it came, so the graph, which
// keeps doc_uuid lower case, said no such doc and the item read as gone.
func TestDocQueryAsksForUUIDsAsTheyAreWritten(t *testing.T) {
	q := DocQuery([]string{"0D0C0000-0000-4000-8000-000000000001", "0d0c0000-0000-4000-8000-000000000001", `x"]) { uid } q(func: has(doc_uuid`})
	if !strings.Contains(q, `eq(doc_uuid, ["0d0c0000-0000-4000-8000-000000000001"])`) {
		t.Fatalf("want the id once, lower case, and nothing else in the list:\n%s", q)
	}
}
