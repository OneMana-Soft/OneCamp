package domain

import (
	"context"
	"testing"
)

// The edge is written into the query, so only the three reactions edges are
// ever accepted, and anything else is refused before the graph is asked.
func TestGetReactionOnParentReadsOnlyThroughAReactionsEdge(t *testing.T) {
	for _, edge := range []string{"", "doc_title", "~comment_reactions", "comment_reactions @filter(has(doc_title))", "Comment_reactions"} {
		if got, err := GetReactionOnParent(context.Background(), edge, "0x1", "0x2"); err == nil || got != nil {
			t.Errorf("edge %q: %+v, %v; want refused", edge, got, err)
		}
	}
}
