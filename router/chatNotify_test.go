package router

import (
	"os"
	"strings"
	"testing"
)

// "Why did or didn't I get this?" (A4 H3) is routed on the member group
// alone: a signed-in person's own test and own record, never through the
// root. The demo's shared visitor sends no test: their devices are every
// visitor's.
func TestTheNotifyRoutesAreTheMembersOwn(t *testing.T) {
	src, err := os.ReadFile("chatNotify.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, route := range []string{
		`rt.Member.With(customMiddleware.NoDeviceTestsInDemo).Post("/notify/test", notificationController.SendMeATest)`,
		`rt.Member.Get("/notify/why/{message_id}", notificationController.WhyAmISeeingThis)`,
	} {
		if n := strings.Count(body, route); n != 1 {
			t.Errorf("%s is routed %d times", route, n)
		}
	}
	if n := strings.Count(body, "notificationController."); n != 2 {
		t.Errorf("chatNotify.go routes %d handlers, want the two above", n)
	}
	programme, err := os.ReadFile("chatProgramme.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(programme), "\tmountChatNotify(rt)") {
		t.Error("mountChatProgramme doesn't mount the notify routes")
	}
}
