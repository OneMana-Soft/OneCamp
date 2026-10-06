package business

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
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
