package email

import (
	"html/template"
	"strings"
	"testing"
)

// wrote is text someone typed: it looks like HTML and holds a bare "&".
const wrote = `<a href="https://x">y</a> & <img src=z> <style>p{}</style>`

// shownOnce checks an email's HTML part shows wrote as text, escaped once:
// none of it is HTML in the email, and nothing in the email is escaped twice.
func shownOnce(t *testing.T, page string) {
	t.Helper()
	for _, raw := range []string{`<a href="https://x"`, "<img src=z>", "<style>p{}", "</style>"} {
		if strings.Contains(page, raw) {
			t.Errorf("the HTML part holds %q as HTML, not as the text someone wrote:\n%s", raw, page)
		}
	}
	if !strings.Contains(page, template.HTMLEscapeString(wrote)) {
		t.Errorf("the HTML part doesn't show %q as text:\n%s", wrote, page)
	}
	for _, twice := range []string{"&amp;amp;", "&amp;lt;", "&amp;gt;", "&amp;#34;", "&amp;#39;"} {
		if strings.Contains(page, twice) {
			t.Errorf("the HTML part escapes something twice (%q):\n%s", twice, page)
		}
	}
}

// An email shows what people wrote as plain text: their words in the body,
// the headline, the line under it and their name are shown as text in the
// HTML part, once, and as written in the plain-text part.
func TestEmailShowsWhatPeopleWroteAsPlainText(t *testing.T) {
	page, text, err := Render(TemplateData{
		Title:     "Maya moved " + wrote,
		Subtitle:  "R&D · " + wrote,
		ActorName: "Maya " + wrote,
		Body:      "said " + wrote,
		CTAURL:    "https://team.example.test/app/task/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	shownOnce(t, page)
	if !strings.Contains(page, "said "+template.HTMLEscapeString(wrote)) {
		t.Errorf("the HTML part doesn't show the body's text:\n%s", page)
	}
	if n := strings.Count(page, template.HTMLEscapeString(wrote)); n < 5 {
		t.Errorf("the HTML part shows the text %d times, want it in the title, the headline, the line under it, the name and the body:\n%s", n, page)
	}
	for _, want := range []string{"Maya moved " + wrote, "R&D · " + wrote, "Maya " + wrote + ":", "said " + wrote} {
		if !strings.Contains(text, want) {
			t.Errorf("the plain-text part doesn't show %q as written:\n%s", want, text)
		}
	}
	for _, escaped := range []string{"&lt;", "&gt;", "&amp;", "&#34;", "&#39;"} {
		if strings.Contains(text, escaped) {
			t.Errorf("the plain-text part shows %q instead of what was written:\n%s", escaped, text)
		}
	}
}

// A body of several lines keeps them in both parts: each line break is a
// <br> in the HTML part, and what was written stays text, escaped once.
func TestEmailBodyKeepsItsLines(t *testing.T) {
	body := "<b>x</b>\nPhone: 1\r\nNote: +1 & \"y\"\rend"
	page, text, err := Render(TemplateData{Title: "Booked", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if want := "&lt;b&gt;x&lt;/b&gt;<br>Phone: 1<br>Note: &#43;1 &amp; &#34;y&#34;<br>end"; !strings.Contains(page, want) {
		t.Errorf("the HTML part doesn't show the body's lines as %q:\n%s", want, page)
	}
	if strings.Contains(page, "<b>x") || strings.Contains(page, "\r") {
		t.Errorf("the HTML part holds the body as HTML, or a bare line break:\n%s", page)
	}
	if !strings.Contains(text, body) {
		t.Errorf("the plain-text part changed the body:\n%q", text)
	}
	if got := TextHTML("a\n<i>b</i>\x00"); got != "a<br>&lt;i&gt;b&lt;/i&gt;\uFFFD" {
		t.Errorf("TextHTML: %q", got)
	}
}
