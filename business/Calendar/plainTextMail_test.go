package Calendar

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A guest's invitation shows what the organiser wrote as plain text: the
// meeting's place and notes are text in the HTML part, escaped once, while
// the lines, the answers and their links stay the email's own formatting; the
// plain-text part and the subject show them as written.
func TestAnInvitationShowsWhatPeopleWroteAsPlainText(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	h.people[sam.UserDgraphInfo.Uuid].EmailID = "sam@acme.example"
	mail := captureMail(t, true)
	start, end := timed(time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC), 45, "Europe/London")
	in := EventInput{Title: "Q&A <b>sync</b>", Start: start, End: end, Location: `R&D <img src=z> room`,
		Notes:     "Bring the deck\n<a href=\"https://x\">y</a> & <style>p{}</style>",
		Attendees: []GuestInput{{Email: "Alex Kim <alex@example.org>"}}}
	if _, err := h.s.Create(ctx, sam, in); err != nil {
		t.Fatal(err)
	}
	h.s.tellOutsiders(ctx, h.tells.of(toldMade)[0], true)
	if len(*mail) != 1 {
		t.Fatalf("%d emails", len(*mail))
	}
	m := (*mail)[0]
	for _, raw := range []string{"<img src=z>", `<a href="https://x"`, "<style>", "<b>sync</b>"} {
		if strings.Contains(m.html, raw) {
			t.Errorf("the HTML part holds %q as HTML:\n%s", raw, m.html)
		}
	}
	for _, shown := range []string{"Where: R&amp;D &lt;img src=z&gt; room", "&lt;a href=&#34;https://x&#34;&gt;y&lt;/a&gt; &amp; &lt;style&gt;p{}&lt;/style&gt;",
		"Bring the deck<br>", `?answer=yes">Yes</a>`, "<strong>Going?</strong>"} {
		if !strings.Contains(m.html, shown) {
			t.Errorf("the HTML part has no %q:\n%s", shown, m.html)
		}
	}
	if strings.Contains(m.html, "&amp;amp;") || strings.Contains(m.html, "&amp;lt;") {
		t.Errorf("the HTML part escapes something twice:\n%s", m.html)
	}
	for _, want := range []string{"Where: R&D <img src=z> room", `<a href="https://x">y</a> & <style>p{}</style>`} {
		if !strings.Contains(m.text, want) {
			t.Errorf("the plain-text part doesn't show %q as written:\n%s", want, m.text)
		}
	}
	if strings.Contains(m.text, "&lt;") || strings.Contains(m.text, "&amp;") || strings.Contains(m.text, "<br>") {
		t.Errorf("the plain-text part shows escapes or the email's own tags:\n%s", m.text)
	}
	if !strings.HasPrefix(m.subject, "Invitation: Q&A <b>sync</b> · ") {
		t.Errorf("the subject isn't the title as written: %q", m.subject)
	}
}
