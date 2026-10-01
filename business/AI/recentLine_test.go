package business

import "testing"

// Tasks were indexed without an author and every item was written as a
// message, so the assistant reported that "Unknown scheduled a launch retro".
func TestRecentItemsReadAsWhatTheyAre(t *testing.T) {
	cases := []struct{ kind, author, want string }{
		{"post", "Maya Chen", "@Maya Chen: "},
		{"chat", "", "[message] "},
		{"task", "Sam Rivera", "[task by Sam Rivera] "},
		{"task", "", "[task] "},
		{"doc", "Sam Rivera", "[doc by Sam Rivera] "},
	}
	for _, c := range cases {
		if got := recentLinePrefix(c.kind, c.author); got != c.want {
			t.Errorf("recentLinePrefix(%q, %q) = %q, want %q", c.kind, c.author, got, c.want)
		}
		if got := recentLinePrefix(c.kind, c.author); got == "@Unknown: " {
			t.Errorf("%s with no author still reads as Unknown", c.kind)
		}
	}
}
