package liveness

import (
	"strings"
	"testing"
)

const (
	liveDoc  = "8611f10f-70f7-462e-b966-ab5c89423881"
	goneDoc  = "f2b3b6ff-0000-4000-8000-000000000001"
	trashDoc = "f2b3b6ff-0000-4000-8000-000000000002"
)

type hit struct{ t, id string }

func hitKey(h hit) (string, string) { return h.t, h.id }

// Gone is dropped and reported; soft-deleted is dropped but not reported;
// unchecked types and odd ids pass through.
func TestPartition(t *testing.T) {
	states := map[string]map[string]bool{"doc": {liveDoc: true, trashDoc: false}}
	in := []hit{{"doc", liveDoc}, {"doc", goneDoc}, {"doc", strings.ToUpper(trashDoc)}, {"user", goneDoc}, {"doc", "not-a-uuid"}}
	kept, gone := Partition(in, hitKey, states)
	if len(kept) != 3 || kept[0].id != liveDoc || kept[1].t != "user" || kept[2].id != "not-a-uuid" {
		t.Fatalf("kept %v", kept)
	}
	if len(gone) != 1 || gone[0].id != goneDoc {
		t.Fatalf("gone %v", gone)
	}
}

func TestTheDocQueryOnlyEverHoldsUUIDs(t *testing.T) {
	q := DocQuery([]string{liveDoc, `x"]) { uid } } { evil(func: has(user_email`})
	if strings.Contains(q, "evil") || !strings.Contains(q, `"`+liveDoc+`"`) {
		t.Fatalf("query built from unchecked input: %s", q)
	}
}

// A live doc carries a zero doc_deleted_at, so "has a deletion time" is not
// "deleted": the query must use the code base's own filter, and a doc found
// but not in the deleted list is live.
func TestALiveDocWithAZeroDeletionTimeIsLive(t *testing.T) {
	q := DocQuery([]string{liveDoc})
	if !strings.Contains(q, `gt(doc_deleted_at, "1970-01-01T00:00:00Z")`) {
		t.Fatalf("deleted docs are not found with the code base's filter: %s", q)
	}
	states := DocStates([]string{liveDoc, trashDoc}, []string{strings.ToUpper(trashDoc)})
	if !states[liveDoc] {
		t.Fatalf("a doc that is not deleted read as deleted")
	}
	if live, found := states[trashDoc]; !found || live {
		t.Fatalf("a deleted doc read as live or missing")
	}
	if _, found := states[goneDoc]; found {
		t.Fatalf("a doc that does not exist read as found")
	}
}
