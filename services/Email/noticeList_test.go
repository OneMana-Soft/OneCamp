package email

import (
	"strings"
	"testing"
)

// A list email says each notice in order, as text, in the shell every email
// has (its footer and unsubscribe link), and how many more there are.
func TestANoticeListSaysEachNoticeAsText(t *testing.T) {
	d := NoticeList{
		TemplateData: TemplateData{RecipientName: "bo@example.com", Title: "3 new notices", UnsubscribeURL: "https://api.example.com/unsubscribe?t=abc", CTAText: "Open OneCamp", CTAURL: "https://app.example.com/app"},
		Items: []NoticeItem{
			{Title: "Ada sent you a message", Place: "Direct message", Snippet: "the launch plan is ready", Link: "https://app.example.com/app/chat/1"},
			{Title: "<b>Cy</b> mentioned you", Place: "#launch", Snippet: `<img src=x onerror=alert(2)>`, Link: "javascript:alert(1)"},
		},
		More: 3,
	}
	got, text, err := RenderNoticeList(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<b>Cy</b>", "<img", `href="javascript:`} {
		if strings.Contains(got, bad) {
			t.Errorf("the list's HTML holds %q live", bad)
		}
	}
	first, second := strings.Index(got, "the launch plan is ready"), strings.Index(got, "&lt;b&gt;Cy&lt;/b&gt; mentioned you")
	if first < 0 || second < first {
		t.Errorf("the list's HTML doesn't say both notices, in order")
	}
	if !strings.Contains(got, `href="https://app.example.com/app/chat/1"`) {
		t.Errorf("the first notice isn't a link to it")
	}
	for _, want := range []string{"Ada sent you a message", "Direct message", "the launch plan is ready", "https://app.example.com/app/chat/1", "<b>Cy</b> mentioned you", "and 3 more", "https://api.example.com/unsubscribe?t=abc"} {
		if !strings.Contains(text, want) {
			t.Errorf("the list's plain text doesn't say %q", want)
		}
	}
	if !strings.Contains(got, "and 3 more") || !strings.Contains(got, "https://api.example.com/unsubscribe?t=abc") {
		t.Errorf("the list's HTML lacks the count of the rest or the shell's unsubscribe link")
	}
}
