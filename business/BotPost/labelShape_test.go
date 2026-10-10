package botpost

import (
	"strings"
	"testing"
)

// TestLabelledHTMLSurvivesPosting pins the stored form of a relayed person's
// message, a channel guest's or a Slack person's: LabelledHTML over an HTML
// body, then the sanitising pass PostToChannelAsBot and
// PostCommentToPostAsBot give it. The web app reads the person's name back
// out of that first bold paragraph (lib/relayedAuthor.ts) and shows them as
// the author; if posting reshaped it, members would see the bot's name and a
// bracket line again.
func TestLabelledHTMLSurvivesPosting(t *testing.T) {
	cases := []struct{ body, label, want string }{
		{"<p>Looks good, ship it</p>", "Priya (Acme) (guest)", "<p><strong>[Priya (Acme) (guest)]</strong></p><p>Looks good, ship it</p>"},
		{"<p>one<br/>two</p>", "Ana Ruiz", "<p><strong>[Ana Ruiz]</strong></p><p>one<br/>two</p>"},
		{"<pre>code</pre>", "Ana", "<p><strong>[Ana]</strong></p><pre>code</pre>"},
	}
	for _, c := range cases {
		if got := badgeHTML(LabelledHTML(c.body, c.label), ""); got != c.want {
			t.Errorf("%q by %q\n got  %s\n want %s", c.body, c.label, got, c.want)
		}
	}
	// The name is escaped, and the label holds no tag: the web app reads it up
	// to the first "<".
	got := badgeHTML(LabelledHTML("<p>hi</p>", "O'Neil & <b>Co</b>"), "")
	rest, ok := strings.CutPrefix(got, "<p><strong>[")
	label, _, closed := strings.Cut(rest, "]</strong></p>")
	if !ok || !closed || strings.Contains(label, "<") || !strings.Contains(label, "&lt;b&gt;") {
		t.Fatalf("label not escaped: %s", got)
	}
}
