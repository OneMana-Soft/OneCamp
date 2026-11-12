package domain

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
)

// The Threads count asks which roots are live in ONE read of the graph: a
// block for the channels' roots and one for the conversations', each keeping
// only the live ones by the shared boundary, and no read when it names none.
func TestLiveIsOneReadForBothKinds(t *testing.T) {
	p, c := `"00000000-0000-4000-8000-000000000001"`, `"00000000-0000-4000-8000-000000000002"`
	q := liveQuery(p, 1, c, 1)
	if strings.Count(q, "(func:") != 2 || !strings.HasPrefix(q, "{") || strings.Count(q, "{") != 3 {
		t.Fatalf("not one query of two blocks: %s", q)
	}
	for _, want := range []string{
		"p(func: eq(post_uuid, [" + p + "]), first: 1) @filter(type(Post) AND " + dgraphquery.Live("post_deleted_at") + ") { id: post_uuid }",
		"c(func: eq(chat_uuid, [" + c + "]), first: 1) @filter(type(Chat) AND " + dgraphquery.Live("chat_deleted_at") + ") { id: chat_uuid }",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("the query lacks %s:\n%s", want, q)
		}
	}
	if q := liveQuery(p, 1, "", 0); strings.Count(q, "(func:") != 1 || strings.Contains(q, "chat_uuid") {
		t.Errorf("posts alone: %s", q)
	}
	if q := liveQuery("", 0, c, 1); strings.Count(q, "(func:") != 1 || strings.Contains(q, "post_uuid") {
		t.Errorf("chats alone: %s", q)
	}
	if q := liveQuery("", 0, "", 0); q != "" {
		t.Errorf("no ids still reads: %s", q)
	}
}
