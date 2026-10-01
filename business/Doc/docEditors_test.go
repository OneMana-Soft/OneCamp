package businness

import "testing"

// docEditors decides who can open a document at creation. The meeting notes
// agent is the only caller that passes more than one person, and it passes a
// speaker list assembled from a call transcript, so this is the function that
// decides whether a meeting document reaches the right people and nobody else.
func TestDocEditors(t *testing.T) {
	t.Run("the creator is always included, exactly once", func(t *testing.T) {
		got := docEditors("0xcreator", []string{"0xcreator", "0xa"})
		if len(got) != 2 {
			t.Fatalf("got %d editors, want 2 (creator deduplicated)", len(got))
		}
		if got[0].Uid != "0xcreator" {
			t.Errorf("first editor = %q, want the creator", got[0].Uid)
		}
	})

	t.Run("duplicates collapse", func(t *testing.T) {
		// A speaker list comes from transcript lines, so the same person appears
		// once per utterance before it is deduplicated upstream. Relying on that
		// would make this function's correctness someone else's problem.
		got := docEditors("0xc", []string{"0xa", "0xa", "0xb", "0xa"})
		if len(got) != 3 {
			t.Errorf("got %d editors, want 3", len(got))
		}
	})

	t.Run("empty uids are dropped, not written", func(t *testing.T) {
		// An empty uid in a Dgraph edge is a dangling reference that resolves to
		// nothing and reports nothing.
		got := docEditors("0xc", []string{"", "   ", "0xa"})
		for _, u := range got {
			if u.Uid == "" {
				t.Fatal("an empty uid reached the editing set")
			}
		}
		if len(got) != 2 {
			t.Errorf("got %d editors, want 2", len(got))
		}
	})

	t.Run("no extras is the ordinary single-owner document", func(t *testing.T) {
		if got := docEditors("0xc", nil); len(got) != 1 || got[0].Uid != "0xc" {
			t.Errorf("got %v, want just the creator", got)
		}
	})

	t.Run("a creator with no uid never yields an unopenable document", func(t *testing.T) {
		// Empty in, empty out: the caller must treat this as "do not create",
		// which createMeetingNotesDoc does by returning before it gets here.
		if got := docEditors("", nil); len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})
}
