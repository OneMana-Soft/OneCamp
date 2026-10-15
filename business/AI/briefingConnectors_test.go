package business

import "testing"

func TestSenderName(t *testing.T) {
	cases := map[string]string{
		"Sarah Lee <sarah@example.com>": "Sarah Lee",
		"<bob@example.com>":             "bob@example.com",
		"alice@example.com":             "alice@example.com",
		`"Quoted Name" <q@example.com>`: "Quoted Name",
		"":                              "",
	}
	for in, want := range cases {
		if got := senderName(in); got != want {
			t.Errorf("senderName(%q) = %q, want %q", in, got, want)
		}
	}
}
