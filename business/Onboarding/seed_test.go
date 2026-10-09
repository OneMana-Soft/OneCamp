package business

import (
	"strings"
	"testing"
)

// The welcome post is what every new member opens on (they are put in
// #general as they join), so it must not hand them an admin's to-do list:
// setting up email or a model provider is the admin checklist's job, and on
// OneCamp Cloud it is done for them. It used to say both, to everyone.
func TestWelcomeCopyGivesNobodySetupWork(t *testing.T) {
	html := strings.ToLower(welcomeHTML())
	for _, setup := range []string{"email", "model provider", "local model", "before you invite", "set up"} {
		if strings.Contains(html, setup) {
			t.Errorf("the welcome post tells its readers to do setup work (%q):\n%s", setup, html)
		}
	}
}

// The AI-free edition does not contain the AI packages at all, and a workspace
// with AI may not have it configured, so the post names no AI feature at all.
func TestWelcomeCopyNeverMentionsAI(t *testing.T) {
	html := strings.ToLower(welcomeHTML())
	for _, banned := range []string{" ai ", "assistant", "agent", "leaves your server"} {
		if strings.Contains(html, banned) {
			t.Errorf("welcome copy contains %q:\n%s", banned, html)
		}
	}
}

// New members land here unless an admin chose other channels
// (channelBusiness.JoinDefaultChannels), and the post says exactly that, not
// that everyone does; and it asks the newcomer for the one thing they can do.
func TestWelcomeCopyIsWrittenForTheNewcomer(t *testing.T) {
	html := strings.ToLower(welcomeHTML())
	if strings.Contains(html, "everyone who joins") {
		t.Errorf("the post says everyone who joins lands here, which an admin's choice of channels makes untrue:\n%s", html)
	}
	for _, want := range []string{"new members are added here unless an admin has chosen other channels", "say hello", "browse channels"} {
		if !strings.Contains(html, want) {
			t.Errorf("welcome copy does not say %q:\n%s", want, html)
		}
	}
}

// Seeded content is the first thing a new teammate reads. House style bans em
// and en dashes, and a stray one here is on the most-read screen in the app.
func TestWelcomeCopyUsesNoDashes(t *testing.T) {
	html := welcomeHTML()
	for _, dash := range []string{"—", "–", "--"} {
		if strings.Contains(html, dash) {
			t.Errorf("welcomeHTML contains %q", dash)
		}
	}
}

// The post body is rendered as HTML, and an unclosed tag would swallow the
// rest of the channel.
func TestWelcomeCopyTagsAreBalanced(t *testing.T) {
	html := welcomeHTML()
	for _, tag := range []string{"p", "ul", "li"} {
		open := strings.Count(html, "<"+tag+">")
		closed := strings.Count(html, "</"+tag+">")
		if open != closed {
			t.Errorf("<%s> opened %d times, closed %d", tag, open, closed)
		}
	}
}
