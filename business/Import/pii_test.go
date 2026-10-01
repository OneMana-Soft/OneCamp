package business

import "testing"

func TestRedactEmail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single email", "user not found: alice@example.com", "user not found: a***@example.com"},
		{"two emails", "merge a@x.com and b@y.com", "merge a***@x.com and b***@y.com"},
		{"no email", "no addresses here", "no addresses here"},
		{"empty", "", ""},
		// First-character-prefixed redaction stays correlatable while
		// stripping the local-part body.
		{"plus addressing", "alice+filter@example.com", "a***@example.com"},
		// Subdomains kept.
		{"subdomain", "ops@team.acme.example.io", "o***@team.acme.example.io"},
		// At-symbol alone shouldn't match.
		{"at without email", "@mention is fine", "@mention is fine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactEmail(tc.in); got != tc.want {
				t.Errorf("redactEmail(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
