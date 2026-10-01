package business

import "testing"

// A PR comment may continue an agent's run only when it did not come from us.
//
// WHY THIS EXISTS. The event this guards re-drives the agent that opened the PR. The
// original check skipped sender.type == "Bot", which covers a GitHub App — and misses the
// way THIS product actually comments: github_comment posts through the connected person's
// OAuth token, so a comment OneCamp wrote arrives as sender.type "User" and passed the
// guard. An agent holding that tool could comment, be woken by its own comment, comment
// again, and keep going — spending tokens and posting publicly on someone else's
// repository every time round.
//
// getGitHubBotLogin existed for exactly this comparison: written, cached, retried, its
// cache invalidated in two places, and never called. Same shape as every other
// unwired-mechanism defect in this codebase — the helper worked, nothing asked it.
func TestPRCommentMayDriveAgent(t *testing.T) {
	const bot = "onecamp-bot"

	cases := []struct {
		name       string
		senderType string
		commenter  string
		botLogin   string
		want       bool
	}{
		{"a human on the PR drives the agent", "User", "some-reviewer", bot, true},
		{"a GitHub App never does", "Bot", "dependabot[bot]", bot, false},
		{"Bot is matched case-insensitively", "bot", "x", bot, false},
		{"OUR OWN comment via the user's token never does", "User", bot, bot, false},
		{"login match is case-insensitive, as GitHub logins are", "User", "OneCamp-Bot", bot, false},
		{"surrounding whitespace does not defeat the match", "User", " onecamp-bot ", " onecamp-bot ", false},
		// A login we could not resolve must NOT silence every human comment: failing to
		// reach GitHub is not a reason to stop the feature working.
		{"an unresolved bot login still lets humans through", "User", "some-reviewer", "", true},
		{"an unresolved bot login still blocks apps", "Bot", "dependabot[bot]", "", false},
		// A comment with no author cannot be proven ours, so it is treated as external.
		{"a missing commenter login is not assumed to be ours", "User", "", bot, true},
	}

	for _, c := range cases {
		if got := prCommentMayDriveAgent(c.senderType, c.commenter, c.botLogin); got != c.want {
			t.Errorf("%s: prCommentMayDriveAgent(%q, %q, %q) = %v, want %v",
				c.name, c.senderType, c.commenter, c.botLogin, got, c.want)
		}
	}
}

// The loop specifically: our own comment must not re-drive the run, whatever the sender
// type says. Stated separately from the table because it is the defect, not a case.
func TestOurOwnCommentCannotSelfDriveTheAgent(t *testing.T) {
	const bot = "onecamp-bot"
	// This is precisely what github_comment produces: our text, the user's token, so
	// GitHub reports a human sender.
	if prCommentMayDriveAgent("User", bot, bot) {
		t.Error("a comment OneCamp posted through the user's OAuth token must not wake the agent " +
			"that posted it — that is a loop that spends tokens and comments publicly each round")
	}
}
