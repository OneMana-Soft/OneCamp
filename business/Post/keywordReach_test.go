package business

import (
	"context"
	"testing"
)

// A member whose keyword a channel post holds hears of it as of a mention;
// never the writer, never someone it already names (TN-53).
func TestKeywordReachedSkipsWriterAndNamed(t *testing.T) {
	oldM, oldK := channelMembers, keywordHits
	t.Cleanup(func() { channelMembers, keywordHits = oldM, oldK })
	channelMembers = func(_ context.Context, ch string, limit int) ([]string, error) {
		if ch != "ch" || limit <= 0 {
			t.Fatalf("members of %q, limit %d", ch, limit)
		}
		return []string{"writer", "named", "kw", "other"}, nil
	}
	var asked []string
	keywordHits = func(_ context.Context, text string, people []string) (map[string]string, error) {
		asked = people
		return map[string]string{"kw": "launch"}, nil
	}
	got := keywordReached(context.Background(), "ch", "The launch", "WRITER", map[string]bool{"Named": true})
	if len(got) != 1 || got[0] != "kw" {
		t.Fatalf("reached %v", got)
	}
	if len(asked) != 2 || asked[0] != "kw" || asked[1] != "other" {
		t.Fatalf("asked about %v", asked)
	}
}
