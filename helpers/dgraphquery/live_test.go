package dgraphquery

import "testing"

func TestLiveAndDeletedAreTheGraphsOneRule(t *testing.T) {
	if got := Live("doc_deleted_at"); got != `not gt(doc_deleted_at, "1970-01-01T00:00:00Z")` {
		t.Fatalf("live: %s", got)
	}
	if got := Deleted("task_deleted_at"); got != `gt(task_deleted_at, "1970-01-01T00:00:00Z")` {
		t.Fatalf("deleted: %s", got)
	}
	if got := TimeSet("recording_ended_at"); got != `gt(recording_ended_at, "1970-01-01T00:00:00Z")` {
		t.Fatalf("time set: %s", got)
	}
}
