package notification

import "testing"

// A chat email links each reader to the conversation as they see it: one
// link for both sides of a DM was /app/chat/<grouping id>, which the web app
// read as a person and opened empty.
func TestChatEmailsLinkEachReaderToTheConversation(t *testing.T) {
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.test")
	const maya, jonas = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const group = "0123456789abcdef0123456789abcdef"
	dm := jonas + " " + maya
	base := "https://team.example.test"

	dmEvent := Event{CTAURL: frontendChatListURL(), CTAURLFor: chatLinkFor(dm, "")}
	if got := ctaURLFor(dmEvent, maya); got != base+"/app/chat/"+jonas {
		t.Errorf("Maya's copy of a DM email opens %q, want the conversation with Jonas", got)
	}
	if got := ctaURLFor(dmEvent, jonas); got != base+"/app/chat/"+maya {
		t.Errorf("Jonas's copy of a DM email opens %q, want the conversation with Maya", got)
	}
	if got := ctaURLFor(Event{CTAURLFor: chatLinkFor(group, "")}, maya); got != base+"/app/chat/group/"+group {
		t.Errorf("a group email opens %q, want the group", got)
	}
	if got := ctaURLFor(Event{CTAURLFor: chatLinkFor(dm, "m1")}, maya); got != base+"/app/chat/"+jonas+"/m1" {
		t.Errorf("a DM thread reply email opens %q, want its thread", got)
	}
	// Nothing to build a link from: the conversations list, never a broken one.
	if got := ctaURLFor(Event{CTAURL: frontendChatListURL(), CTAURLFor: chatLinkFor("", "")}, maya); got != base+"/app/chat" {
		t.Errorf("a chat email without a grouping id opens %q, want the conversations list", got)
	}
	// Every other event keeps its one link.
	if got := ctaURLFor(Event{CTAURL: base + "/app/task/t1"}, maya); got != base+"/app/task/t1" {
		t.Errorf("an event without per-reader links opens %q", got)
	}
}
