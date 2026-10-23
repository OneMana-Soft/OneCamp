package business

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/microcosm-cc/bluemonday"
	"google.golang.org/api/gmail/v1"
)

func TestEmailHTMLIsSafeToShow(t *testing.T) {
	in := `<p onclick="x()">Hi <b>there</b></p>
<script>alert(1)</script>
<img src="https://tracker.example/open.gif?u=42" width=1 height=1>
<img src='//cdn.example/pixel.png'>
<img src="data:image/png;base64,iVBORw0KGgo=">
<a href="https://example.com/doc">doc</a>
<a href="javascript:alert(1)">bad</a>
<form action="https://evil"><input name=p></form>
<div style="position:fixed;top:0">overlay</div>`
	out := SanitizeEmailHTML(in)
	for _, banned := range []string{"<script", "onclick", "tracker.example", "cdn.example", "javascript:", "<form", "<input", "position:fixed"} {
		if strings.Contains(out, banned) {
			t.Errorf("kept %q:\n%s", banned, out)
		}
	}
	for _, kept := range []string{"<b>there</b>", `href="https://example.com/doc"`, `target="_blank"`, "noreferrer", "data:image/png"} {
		if !strings.Contains(out, kept) {
			t.Errorf("lost %q:\n%s", kept, out)
		}
	}
}

// An email's remote images never load: fetched from the sender's server when
// the email is opened, an image tells them it was read, when, and from which
// address. A regex stripped them before sanitising, and missed a ">" inside an
// attribute and an encoded scheme (audit package 35).
func TestAnEmailsRemoteImagesNeverLoad(t *testing.T) {
	for name, img := range map[string]string{
		"a plain one":             `<img src="https://t.example/open.gif?u=42" width=1 height=1>`,
		"a > inside an attribute": `<img alt=">" src="https://t.example/open.gif">`,
		"an encoded scheme":       `<img src="&#104;ttps://t.example/open.gif">`,
		"no scheme":               `<img src="//t.example/open.gif">`,
		"shouted":                 `<IMG SRC = "HTTPS://T.EXAMPLE/open.gif">`,
		"plain http":              `<img src='http://t.example/open.gif'>`,
	} {
		out := SanitizeEmailHTML("<p>Your order shipped</p>" + img)
		if strings.Contains(strings.ToLower(out), "t.example") {
			t.Errorf("%s: kept the remote image: %s", name, out)
		}
		if !strings.Contains(out, "<p>Your order shipped</p>") {
			t.Errorf("%s: lost the text: %s", name, out)
		}
	}
	inline := `<img src="data:image/png;base64,iVBORw0KGgo=" alt="logo">`
	if out := SanitizeEmailHTML(inline); !strings.Contains(out, `src="data:image/png;base64,iVBORw0KGgo="`) {
		t.Errorf("lost an image inside the email: %s", out)
	}
}

// Images aside, an email keeps what user content keeps (bluemonday's UGCPolicy,
// which emailPolicy follows): text, links, lists and tables.
func TestAnEmailKeepsWhatUserContentKeeps(t *testing.T) {
	in := `<h2 dir="ltr">Q4 launch</h2><p>Hi <b>Sam</b>, <em>see</em> <a href="https://example.com/plan" rel="x">the plan</a>.</p>
<blockquote cite="https://example.com/q">Ship on Friday</blockquote><ul><li>one</li></ul><ol type="a"><li>two</li></ol>
<table><thead><tr><th colspan="2" align="left">Step</th></tr></thead><tbody><tr><td valign="top">Freeze</td><td>Fri</td></tr></tbody></table>
<details open><summary>More</summary><p>Notes <code>v2</code> <del datetime="2026-10-22T10:00:00Z">old</del> <time datetime="2026-10-24">Fri</time></p></details>
<p><abbr title="as soon as possible">ASAP</abbr> <q cite="https://example.com">quoted</q> <sub>1</sub><sup>2</sup> <mark>hi</mark></p>
<pre>  kept   spaces</pre><hr><dl><dt>term</dt><dd>meaning</dd></dl>
<article id="a1" title="t" lang="en"><aside>aside</aside><section><figure><figcaption>cap</figcaption></figure></section></article>
<hgroup><h1>t</h1></hgroup><map name="m"><area shape="rect" coords="0,0,1,1" href="https://example.com/a" alt="x" rel="noopener"></map>
<meter value="0.5" min="0" max="1">half</meter><progress value="1" max="2"></progress><bdo dir="rtl">abc</bdo>
<ruby>kan<rp>(</rp><rt>k</rt><rp>)</rp></ruby><p><small>s</small><strike>x</strike><tt>t</tt><u>u</u><s>s</s><samp>x</samp>
<var>v</var><dfn>d</dfn><acronym>a</acronym><cite>c</cite><i>i</i><span>sp</span><wbr><ins cite="note">new</ins></p>`
	ugc := bluemonday.UGCPolicy()
	ugc.RequireNoReferrerOnLinks(true)
	ugc.AddTargetBlankToFullyQualifiedLinks(true)
	if got, want := SanitizeEmailHTML(in), ugc.Sanitize(in); got != want {
		t.Fatalf("an email keeps\n%s\nwhere user content keeps\n%s", got, want)
	}
}

func TestPlainTextIsEscaped(t *testing.T) {
	got := helpers.PlainTextToHTML("a < b\r\n<script>x</script>")
	if got != "<p>a &lt; b<br>&lt;script&gt;x&lt;/script&gt;</p>" {
		t.Fatalf("got %q", got)
	}
}

func enc(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

func TestMessageBodyPrefersHTMLAndBoundsSize(t *testing.T) {
	part := &gmail.MessagePart{MimeType: "multipart/alternative", Parts: []*gmail.MessagePart{
		{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: enc("plain")}},
		{MimeType: "text/html; charset=utf-8", Body: &gmail.MessagePartBody{Data: enc("<p>rich</p>")}},
		{MimeType: "application/pdf", Filename: "invoice.pdf", Body: &gmail.MessagePartBody{AttachmentId: "a1"}},
	}}
	body, cut := messageBody(part)
	if body != "<p>rich</p>" || cut {
		t.Fatalf("body=%q truncated=%v", body, cut)
	}
	if countAttachments(part) != 1 {
		t.Fatal("the attachment was not counted")
	}
	big := &gmail.MessagePart{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: enc(strings.Repeat("x", maxBodyBytes+10))}}
	if _, cut := messageBody(big); !cut {
		t.Fatal("an oversized body was not truncated")
	}
}

func TestReplyGoesToTheRightPerson(t *testing.T) {
	me := "sam@acme.com"
	if got := replyRecipient(me, "Priya <priya@x.com>", "", me); got != "Priya <priya@x.com>" {
		t.Errorf("reply to sender: %q", got)
	}
	if got := replyRecipient(me, "Priya <priya@x.com>", "support@x.com", me); got != "support@x.com" {
		t.Errorf("Reply-To ignored: %q", got)
	}
	if got := replyRecipient(me, "Sam <SAM@acme.com>", "", "priya@x.com"); got != "priya@x.com" {
		t.Errorf("after my own message the reply must go to its recipients: %q", got)
	}
}

func TestLineBreaksCannotAddHeaders(t *testing.T) {
	for _, v := range []string{"a@x.com\r\nBcc: spy@evil", "Subject\nX-Evil: 1"} {
		if !hasLineBreak(v) {
			t.Errorf("missed %q", v)
		}
	}
	if hasLineBreak("Priya <priya@x.com>") {
		t.Error("a normal address was refused")
	}
	if !reThreadID.MatchString("18c2f3a9b4d5e6f7") || reThreadID.MatchString("../../etc") {
		t.Error("thread id check is wrong")
	}
}
