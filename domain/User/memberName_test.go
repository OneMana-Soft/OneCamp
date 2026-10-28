package domain

import "testing"

func TestMemberNameIsANameNeverAnAddress(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{"  Maya Patel ", "maya"}, "Maya Patel"},
		{[]string{"", "maya"}, "maya"},
		{[]string{"maya@acme.test", "Maya"}, "Maya"},
		{[]string{"sam@acme.test"}, ""},
		{[]string{"   "}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := MemberName(c.names...); got != c.want {
			t.Errorf("MemberName(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}
