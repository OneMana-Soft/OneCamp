package business

import (
	"errors"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
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

// TestGuestMessageHTMLLabelShape pins the stored form the web app reads a
// guest's name and words back out of (lib/relayedAuthor.ts, splitRelayLabel):
// the label as a bold paragraph of its own, escaped, then their text. Change
// one and the other must change with it, or members see "Guests" again with
// the guest's name as the first line of the message.
func TestGuestMessageHTMLLabelShape(t *testing.T) {
	got, err := GuestMessageHTML("Priya (Acme)", "Looks good, ship it")
	if err != nil {
		t.Fatal(err)
	}
	if want := "<p><strong>[Priya (Acme) (guest)]</strong></p><p>Looks good, ship it</p>"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	got, _ = GuestMessageHTML("O'Neil & <Co>", "hi")
	if !strings.HasPrefix(got, "<p><strong>[O&#39;Neil &amp; &lt;Co&gt; (guest)]</strong></p>") {
		t.Fatalf("the label is escaped and holds no tag: %s", got)
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

func TestAGuestIsNamedAsAGuest(t *testing.T) {
	for in, want := range map[string]string{
		"Priya":          "Priya (guest)",
		"  Priya\n Rao ": "Priya Rao (guest)",
		"":               "A guest",
		"Guest":          "A guest",
	} {
		if got := guestLabel(in); got != want {
			t.Errorf("guestLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A guest sees a member by the name rule, display name first, and never part
// of their address.
func TestAGuestSeesAMemberByTheNameRule(t *testing.T) {
	if got := authorName(&dgraphStruct.DgraphUser{UserName: "Sam", UserFullName: "Samuel Rivera"}); got != "Sam" {
		t.Fatalf("got %q, want the display name", got)
	}
	if got := authorName(&dgraphStruct.DgraphUser{EmailID: "sam@example.com"}); got != "Someone" {
		t.Fatalf("got %q, want Someone rather than part of an address", got)
	}
}
