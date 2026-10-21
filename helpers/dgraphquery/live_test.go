package dgraphquery

import "testing"

func TestLiveAndDeletedAreTheGraphsOneRule(t *testing.T) {
	if got := Live("doc_deleted_at"); got != `not gt(doc_deleted_at, "1970-01-01T00:00:00Z")` {
		t.Fatalf("live: %s", got)
	}
	if got := Deleted("task_deleted_at"); got != `gt(task_deleted_at, "1970-01-01T00:00:00Z")` {
		t.Fatalf("deleted: %s", got)
	}
}
