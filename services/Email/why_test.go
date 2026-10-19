package email

import (
	"strings"
	"testing"
)

// Someone emailed without having subscribed to anything (a guest invited to a
// meeting) is told why it came, not that they subscribed to notifications.
func TestAnEmailToSomeoneWhoNeverSubscribedSaysWhyItCame(t *testing.T) {
	why := "Maya Chen invited you to this meeting from OneCamp."
	html, text, err := Render(TemplateData{Title: "Design review", Why: why})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, why) || strings.Contains(html, "subscribed to notifications") {
		t.Errorf("the HTML should say why, not that they subscribed:\n%s", html)
	}
	if !strings.Contains(text, why) {
		t.Errorf("the text should say why:\n%s", text)
	}
	html, _, err = Render(TemplateData{Title: "A mention"})
	if err != nil || !strings.Contains(html, "subscribed to notifications") {
		t.Errorf("a notification still says why it came: %v", err)
	}
}
