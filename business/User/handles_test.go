package business

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
)

// Two people with one name share it, and get handles sam and sam-2; one taken
// in another case counts as taken.
func TestTheFirstFreeHandleIsNumbered(t *testing.T) {
	cases := []struct {
		taken []string
		want  string
	}{
		{nil, "sam"},
		{[]string{"sam"}, "sam-2"},
		{[]string{"Sam", "sam-2"}, "sam-3"},
		{[]string{"sam-2"}, "sam"},
		{[]string{"sam", "sam-3", "sam-2", "sam-x"}, "sam-4"},
	}
	for _, c := range cases {
		if got := firstFreeHandle("sam", c.taken); got != c.want {
			t.Errorf("taken %v: got %q, want %q", c.taken, got, c.want)
		}
	}
}

// Whatever a provider supplies, a new member has a display name the rule
// accepts: cleaned, else from the address, else "Member".
func TestANewMemberAlwaysHasAName(t *testing.T) {
	cases := []struct{ name, email, want string }{
		{"José O'Brien", "jo@example.test", "José O'Brien"},
		{"octo_cat", "octo@example.test", "octo cat"},
		{"", "priya.raman@example.test", "priya.raman"},
		{"___", "___@example.test", "Member"},
	}
	for _, c := range cases {
		if got := memberDisplayName(c.name, c.email); got != c.want {
			t.Errorf("memberDisplayName(%q, %q) = %q, want %q", c.name, c.email, got, c.want)
		}
	}
}

// Someone called Admin is @admin-2: a reserved word is passed over like a
// taken handle. And the handle settled for after lost races fits the 30
// characters any handle may have.
func TestReservedHandlesArePassedOverAndFallbacksFit(t *testing.T) {
	if got := firstFreeHandle("admin", nil); got != "admin-2" {
		t.Errorf("firstFreeHandle(admin) = %q", got)
	}
	if got := firstFreeHandle("everyone", []string{"everyone-2"}); got != "everyone-3" {
		t.Errorf("firstFreeHandle(everyone) = %q", got)
	}
	long := strings.Repeat("a", helpers.HandleMaxRunes)
	got := fallbackHandle(long, "1a2b3")
	if utf8.RuneCountInString(got) > helpers.HandleMaxRunes || !strings.HasSuffix(got, "-1a2b3") || !helpers.IsValidHandle(got) {
		t.Errorf("fallbackHandle = %q (%d characters)", got, utf8.RuneCountInString(got))
	}
}
