package whoKnowsDomain

import (
	"strings"
	"testing"

	docDomain "github.com/akashc777/OneCamp/domain/Doc"
)

// TestCountCreditsPagesOnly (docs F1, DGF1): writing a page is knowing about
// it; uploading a file, making a folder or adding a link isn't. The doc
// block takes pages only (domain/Doc's PageOnly), and every block after it,
// the evidence too, reads its pages (uid(D)), so a folder's, a file's or a
// link's maker is never "wrote", a comment on one never "commented", and none
// is evidence. Tasks and posts are read as before.
func TestCountCreditsPagesOnly(t *testing.T) {
	q := CountQuery(Items{Docs: []string{d1}, Tasks: []string{t1}, Messages: []string{p1}})
	line := func(prefix string) string {
		for _, l := range strings.Split(q, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), prefix) {
				return l
			}
		}
		return ""
	}
	if l := line("D as var(func: eq(doc_uuid"); !strings.Contains(l, docDomain.PageOnly) {
		t.Errorf("the doc block takes every kind of doc, not pages only: %q\n%s", l, q)
	}
	if line("ev_doc(func: uid(D))") == "" {
		t.Errorf("the doc evidence doesn't read the doc block's pages:\n%s", q)
	}
	for _, block := range []string{"T as var(func: eq(task_uuid", "P as var(func: eq(post_uuid", "ev_task(func: uid(T))", "ev_message(func: uid(P))"} {
		if l := line(block); l == "" || strings.Contains(l, docDomain.PageOnly) {
			t.Errorf("the block %q: %q\n%s", block, l, q)
		}
	}
	if n := strings.Count(q, docDomain.PageOnly); n != 1 {
		t.Errorf("PageOnly is written %d times, want once, on the doc block every other block reads:\n%s", n, q)
	}
}
