package domain

import (
	"strings"
	"testing"
)

// A comment read for nobody (an import reads as nobody) leaves out its
// per-viewer counts and the $userId they need: the graph refuses uid(""), and
// the read failed, so an imported reply's reactions, edits and deletion were
// dropped. For a viewer it is as written.
func TestACommentReadForNobodyLeavesOutTheViewersCounts(t *testing.T) {
	const q = `query CommentInfo($id: string, $userId: string){ c(func: eq(comment_uuid, $id)) { n: count(m @filter(uid($userId))) x } }`
	got, vars := forViewer(q, "c1", "", "n: count(m @filter(uid($userId)))")
	if strings.Contains(got, "$userId") || vars["$id"] != "c1" || len(vars) != 1 {
		t.Errorf("for nobody: %q %v", got, vars)
	}
	got, vars = forViewer(q, "c1", "0x9", "n: count(m @filter(uid($userId)))")
	if got != q || vars["$userId"] != "0x9" || vars["$id"] != "c1" {
		t.Errorf("for a viewer: %q %v", got, vars)
	}
}
