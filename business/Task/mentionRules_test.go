package business

import "testing"

// A comment that mentions people who can't see its task is refused in words
// that say who, why, and what to do instead.
func TestAMentionOfSomeoneOutsideTheProjectIsRefusedByName(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"Sam"}, "Sam isn't in this project, so they can't see this task. Take the mention out, or have them added to the project first."},
		{[]string{"Sam", "Lee"}, "Sam and Lee aren't in this project, so they can't see this task. Take the mentions out, or have them added to the project first."},
		{[]string{"Sam", "Lee", "Kim"}, "Sam and 2 others aren't in this project, so they can't see this task. Take the mentions out, or have them added to the project first."},
	} {
		if got := outsidersRefusal(c.names); got != c.want {
			t.Errorf("%v: %q, want %q", c.names, got, c.want)
		}
	}
}
