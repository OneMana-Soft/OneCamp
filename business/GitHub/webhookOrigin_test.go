package business

import (
	"os"
	"strings"
	"testing"
)

// The webhook must mark its context before it applies anything.
//
// Everything below that line updates tasks through the same business functions a
// person's edit goes through, and those enqueue an outbound sync. Without the
// marker, GitHub renaming an issue makes OneCamp PATCH the same title back onto
// the issue it came from: a wasted write against a rate limit already returning
// secondary-limit 403s, and a window where the echo lands on a newer local edit.
//
// Source-level because the alternative is standing up Dgraph, Postgres and a
// GitHub double to observe one context value.
func TestWebhookMarksItsContextAsGitHubOriginated(t *testing.T) {
	src, err := os.ReadFile("githubBusiness.go")
	if err != nil {
		t.Fatalf("read githubBusiness.go: %v", err)
	}
	body := string(src)

	start := strings.Index(body, "func HandleGitHubWebhookEvent(")
	if start < 0 {
		t.Fatal("HandleGitHubWebhookEvent not found")
	}

	mark := strings.Index(body[start:], "helpers.WithGitHubOrigin(ctx)")
	if mark < 0 {
		t.Fatal("the webhook handler does not mark its context as GitHub-originated; " +
			"every inbound change will be echoed back to GitHub")
	}

	// Before the dispatch, or the events handled first are still echoed.
	dispatch := strings.Index(body[start:], "switch eventType {")
	if dispatch >= 0 && mark > dispatch {
		t.Error("the context is marked after the event dispatch, so early cases still echo")
	}
}

// A refresh from GitHub brings the issue's comments into the task, written by
// anyone who can comment on it, so it is marked the same way: nothing it writes
// is sent back, and no comment it brings in steers an agent's work as if a
// person in the workspace had written it (handleTaskCommentForAgent).
//
// Source-level: today every refresh stops before the comments, because the task
// read it starts from has no creator to act as.
func TestRefreshMarksItsContextAsGitHubOriginated(t *testing.T) {
	src, err := os.ReadFile("githubSync.go")
	if err != nil {
		t.Fatalf("read githubSync.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func RefreshFromGitHub(")
	if start < 0 {
		t.Fatal("RefreshFromGitHub not found")
	}
	body = body[start:]
	mark := strings.Index(body, "helpers.WithGitHubOrigin(ctx)")
	comments := strings.Index(body, "syncCommentsFromGitHub(")
	if mark < 0 || comments < 0 || mark > comments {
		t.Fatal("a refresh from GitHub does not mark its context as GitHub-originated before it brings comments in; " +
			"a comment anyone wrote on the issue would count as the task creator's")
	}
}
