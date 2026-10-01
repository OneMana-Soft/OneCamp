package business

import (
	"strings"
	"testing"
)

func TestAppLinkRefs(t *testing.T) {
	html := `<blockquote><p>Great. Then the only thing left is the announcement.</p></blockquote>` +
		`<p>From <a href="https://acme.example/app/channel/c-1/p-2">Sam Rivera's message</a></p>` +
		`<p>See <a href="/app/doc/d-3">the plan</a>, <a href="https://acme.example/app/chat/group/g-4/m-5">this</a>, ` +
		`<a href="https://acme.example/app/chat/u-6/m-7">that</a> and <a href="https://acme.example/app/task/t-8">task</a>. ` +
		`Again <a href="https://acme.example/app/channel/c-1/p-2">the message</a>, ` +
		`and <a href="https://evil.example/login">not ours</a>.</p>`
	got := appLinkRefs(html)
	want := []string{"channel_uuid=c-1 post_uuid=p-2", "doc_uuid=d-3", "group chat id=g-4 message=m-5", "user u-6: message=m-7", "task_uuid=t-8"}
	if len(got) != len(want) {
		t.Fatalf("got %d refs %q, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Fatalf("ref %d = %q, want it to carry %q", i, got[i], w)
		}
	}
}

func TestSynthAssignmentPromptKeepsTheLinkedMessage(t *testing.T) {
	p := synthAssignmentPrompt(map[string]interface{}{
		"name":        "Great",
		"description": `<p>From <a href="https://acme.example/app/channel/c-1/p-2">Sam Rivera's message</a></p>`,
		"task_id":     "t-1",
	})
	if !strings.Contains(p, "channel_uuid=c-1 post_uuid=p-2") {
		t.Fatalf("the prompt lost the linked message:\n%s", p)
	}
}
