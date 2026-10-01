package business

import (
	"strings"
	"testing"
)

// The AI-free edition does not contain the AI packages at all, so copy that
// points at a model provider describes a screen that build does not have. This
// is the backend half of the same rule the frontend guards with noAiOnV1.
func TestWelcomeCopyNeverMentionsAIOnTheAIFreeEdition(t *testing.T) {
	html := strings.ToLower(welcomeHTML(false))

	for _, banned := range []string{"model provider", "local model", " ai ", "leaves your server"} {
		if strings.Contains(html, banned) {
			t.Errorf("AI-free welcome copy contains %q:\n%s", banned, html)
		}
	}
}

// With AI in the build, the provider step is the single highest-value thing an
// owner can do before inviting anyone, so it has to be named.
func TestWelcomeCopyNamesTheProviderStepWhenAIIsBuiltIn(t *testing.T) {
	html := strings.ToLower(welcomeHTML(true))

	if !strings.Contains(html, "model provider") {
		t.Errorf("AI edition welcome copy does not mention connecting a provider:\n%s", html)
	}
}

// Email is the one step both editions share: without it no invitation and no
// password reset can leave the server, which strands every later step.
func TestWelcomeCopyAlwaysNamesEmail(t *testing.T) {
	for _, withAI := range []bool{true, false} {
		if !strings.Contains(strings.ToLower(welcomeHTML(withAI)), "email") {
			t.Errorf("welcomeHTML(%v) does not mention email", withAI)
		}
	}
}

// Seeded content is the first thing a paying customer reads. House style bans
// em and en dashes, and a stray one here is on the most-read screen in the app.
func TestWelcomeCopyUsesNoDashes(t *testing.T) {
	for _, withAI := range []bool{true, false} {
		html := welcomeHTML(withAI)
		for _, dash := range []string{"—", "–", "--"} {
			if strings.Contains(html, dash) {
				t.Errorf("welcomeHTML(%v) contains %q", withAI, dash)
			}
		}
	}
}

// The post has to read as an ordinary post the owner can get rid of, not as
// chrome bolted to the channel. If that line goes, seeded content starts looking
// permanent and the workspace stops feeling like theirs.
func TestWelcomeCopySaysItCanBeDeleted(t *testing.T) {
	for _, withAI := range []bool{true, false} {
		if !strings.Contains(strings.ToLower(welcomeHTML(withAI)), "delete") {
			t.Errorf("welcomeHTML(%v) never tells the owner they can delete it", withAI)
		}
	}
}

// Both editions must produce well-formed markup: the post body is rendered as
// HTML, and an unclosed tag would swallow the rest of the channel.
func TestWelcomeCopyTagsAreBalanced(t *testing.T) {
	for _, withAI := range []bool{true, false} {
		html := welcomeHTML(withAI)
		for _, tag := range []string{"p", "ul", "li"} {
			open := strings.Count(html, "<"+tag+">")
			closed := strings.Count(html, "</"+tag+">")
			if open != closed {
				t.Errorf("welcomeHTML(%v): <%s> opened %d times, closed %d", withAI, tag, open, closed)
			}
		}
	}
}
