package business

import (
	"errors"
	"strings"
	"testing"
)

func TestGuestMessageHTML(t *testing.T) {
	got, err := GuestMessageHTML("  Priya   N ", "Hi <script>x</script>\nsecond line")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "<script>") || !strings.Contains(got, "&lt;script&gt;") || !strings.Contains(got, "<br>second line") || !strings.Contains(got, "Priya N (guest)") {
		t.Fatalf("got %s", got)
	}
	var in *ErrGuestInput
	for _, c := range [][2]string{{"", "hi"}, {"Priya", "   "}, {"Priya", strings.Repeat("x", 4001)}} {
		if _, err := GuestMessageHTML(c[0], c[1]); !errors.As(err, &in) {
			t.Errorf("%q: want an input error, got %v", c, err)
		}
	}
}

func TestPlainText(t *testing.T) {
	got := PlainText(`<p>Ship <strong>Tuesday</strong> &amp; tell <span data-type="mention">@Maya</span></p><p>second</p><br>`)
	if got != "Ship Tuesday & tell @Maya\nsecond" {
		t.Fatalf("got %q", got)
	}
}

func TestSplitLabel(t *testing.T) {
	cases := []struct{ in, author, text string }{
		{"[Priya (Acme) (guest)]\nChanges requested: brighter", "Priya (Acme) (guest)", "Changes requested: brighter"},
		{"[Sam (Slack)] shipped it", "Sam (Slack)", "shipped it"},
		{"no label here", "Guests", "no label here"},
		{"[]", "Guests", "[]"},
	}
	for _, c := range cases {
		if a, txt := splitLabel("Guests", c.in); a != c.author || txt != c.text {
			t.Errorf("%q: got %q / %q", c.in, a, txt)
		}
	}
}
