package business

import "testing"

// A conversation list is only useful if the rows say what they are. "New chat"
// eleven times is a list of nothing, which is why the title is derived from the
// opening question rather than asked for.
func TestTitleFrom(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty gets a name rather than a blank row", "", "New conversation"},
		{"whitespace only", "   \n\t ", "New conversation"},
		{"short question kept whole", "What did we ship last week?", "What did we ship last week?"},
		{
			"stops at the first sentence, which is almost always the subject",
			"Summarise the release. Then compare it to the previous one and note anything that regressed.",
			"Summarise the release.",
		},
		{
			"a pasted multi-line prompt becomes one line",
			"Review this:\n\n  - item one\n  - item two",
			"Review this:",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TitleFrom(c.in); got != c.want {
				t.Fatalf("TitleFrom(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestTitleFromTruncatesOnAWordBoundary(t *testing.T) {
	long := "Please go through every channel in the workspace and tell me which ones have had no activity at all"
	got := TitleFrom(long)

	if len([]rune(got)) > maxTitleLen+1 { // +1 for the ellipsis
		t.Fatalf("title is %d runes, too long for one line: %q", len([]rune(got)), got)
	}
	// Cut mid-word reads as a bug rather than as a truncation.
	if got[len(got)-len("…")-1] == ' ' {
		t.Fatalf("title ends with a dangling space before the ellipsis: %q", got)
	}
	for _, frag := range []string{"workspac…", "activit…"} {
		if got == frag {
			t.Fatalf("title was cut mid-word: %q", got)
		}
	}
	if got[len(got)-len("…"):] != "…" {
		t.Fatalf("a truncated title must show that it was truncated: %q", got)
	}
}
