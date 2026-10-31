package router

import (
	"os"
	"strings"
	"testing"
)

// Each member's own Slack DMs come in only through the member group (no API
// token reaches it), and the demo's shared visitor can't start a read: every
// visitor after them would read the DMs it brought.
func TestOnlyASignedInMemberBringsTheirSlackDMs(t *testing.T) {
	src, err := os.ReadFile("chatSlackDMs.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, route := range []string{
		`rt.Member.Get("/me/slack/dms", slackImportController.HandleMyReads)`,
		`rt.Member.With(customMiddleware.NoPersonalAccountsInDemo).Post("/me/slack/dms", slackImportController.HandleMyRead)`,
		`rt.Member.Get("/me/slack/dms/{jobId}/notes", slackImportController.HandleMyReadNotes)`,
		`rt.Member.Post("/me/slack/dms/{jobId}/stop", slackImportController.HandleStopMyRead)`,
	} {
		if n := strings.Count(body, route); n != 1 {
			t.Errorf("%s is routed %d times", route, n)
		}
	}
	if n := strings.Count(body, "slackImportController."); n != 4 {
		t.Errorf("chatSlackDMs.go routes %d handlers, want the four above", n)
	}
	programme, err := os.ReadFile("chatProgramme.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(programme), "\tmountSlackDMs(rt)") {
		t.Error("mountChatProgramme doesn't mount the member's Slack DMs")
	}
}
