package helpers

import "testing"

// The one name rule: display name, else full name, else the address's part
// before the @, each trimmed.
func TestPersonDisplayName(t *testing.T) {
	cases := []struct{ display, full, email, want string }{
		{"Sam", "Samuel Rivera", "sam@example.com", "Sam"},
		{"  ", "Samuel Rivera", "sam@example.com", "Samuel Rivera"},
		{"", " ", " sam.rivera@example.com", "sam.rivera"},
		{"", "", "", ""},
		{"", "", "no-at-sign", "no-at-sign"},
		{" Sam ", "", "", "Sam"},
	}
	for _, c := range cases {
		if got := PersonDisplayName(c.display, c.full, c.email); got != c.want {
			t.Errorf("PersonDisplayName(%q, %q, %q) = %q, want %q", c.display, c.full, c.email, got, c.want)
		}
	}
}
