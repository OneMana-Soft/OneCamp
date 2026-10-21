package business

import (
	"encoding/json"
	"strings"
	"testing"
)

// A card drawn as a post's body: what an incoming webhook's blocks and an
// app's answer in a channel become. Its words, links and pictures arrive; a
// button that would call its owner doesn't, since nobody can press a post.

func drawn(t *testing.T, blocks string) string {
	t.Helper()
	card, _ := Reduce(json.RawMessage(blocks))
	return HTML(card)
}

func TestACardDrawnAsAPostKeepsItsWordsLinksAndPictures(t *testing.T) {
	got := drawn(t, `[
		{"type": "header", "text": {"type": "plain_text", "text": "Deploy 412"}},
		{"type": "section", "text": {"type": "mrkdwn", "text": "*Done* in _4 min_, see <https://ci.example.com/412|the log>"},
		 "fields": [{"type": "mrkdwn", "text": "*Branch*\nmain"}]},
		{"type": "context", "text": {"type": "plain_text", "text": "From CI"}},
		{"type": "image", "image_url": "https://cdn.example.com/chart.png", "alt_text": "Build times"},
		{"type": "divider"},
		{"type": "actions", "elements": [
			{"type": "button", "text": "Open the run", "url": "https://ci.example.com/412"},
			{"type": "button", "text": "Roll back", "action_id": "rollback"}
		]}
	]`)
	for _, want := range []string{
		"<h3>Deploy 412</h3>",
		"<strong>Done</strong>",
		"<em>4 min</em>",
		`<a href="https://ci.example.com/412" rel="nofollow">the log</a>`,
		"<li><strong>Branch</strong><br/>main</li>",
		"<small>From CI</small>",
		`<img src="https://cdn.example.com/chart.png" alt="Build times"/>`,
		"<hr/>",
		`<a href="https://ci.example.com/412" rel="nofollow">Open the run</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	// A button that would call its owner isn't drawn as one that does nothing.
	if strings.Contains(got, "Roll back") || strings.Contains(got, "<button") {
		t.Errorf("an unpressable button was drawn:\n%s", got)
	}
}

func TestACardsTextIsNeverHTML(t *testing.T) {
	got := drawn(t, `[
		{"type": "section", "text": {"type": "mrkdwn", "text": "<script>alert(1)</script> <javascript:alert(1)|click> <img src=x onerror=alert(1)>"}},
		{"type": "section", "text": {"type": "plain_text", "text": "<b>not bold</b>"}}
	]`)
	if strings.Contains(got, "<script") || strings.Contains(got, `href="javascript`) || strings.Contains(got, "<img src=x") || strings.Contains(got, "<b>") {
		t.Fatalf("markup got through:\n%s", got)
	}
	if !strings.Contains(got, "&lt;b&gt;not bold&lt;/b&gt;") || !strings.Contains(got, "click") {
		t.Fatalf("the words were lost:\n%s", got)
	}
}
