package router

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// On the demo, which holds a real GitHub connection that everyone who opens
// it shares, a task route that writes to GitHub with it is refused, in plain
// words, before it reaches GitHub (the connection refuses any write that gets
// past: business/GitHub workspaceWrites). The task's GitHub routes that only
// read, or only change OneCamp, stay open.
func TestTheDemoRefusesTheTaskRoutesThatWriteToGitHub(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	writes := map[string]bool{
		`.Post("/create-branch/{task_uuid}", taskController.CreateGitHubBranchForTask)`: true,
		`.Post("/github-create-pr/{task_uuid}", taskController.CreateGitHubPRForTask)`:  true,
		`.Post("/github-retry-sync/{task_uuid}", taskController.RetryGitHubSync)`:       true,
	}
	for route := range writes {
		if n := strings.Count(body, route); n != 1 {
			t.Errorf("%s is routed %d times", route, n)
			continue
		}
		if !strings.Contains(body, "r.With(customMiddleware.NoGitHubWritesInDemo)"+route) {
			t.Errorf("%s is not behind NoGitHubWritesInDemo", route)
		}
	}
	// Every task route about GitHub is one of the three, or one that writes
	// nothing to it: a new one has to be placed.
	reads := map[string]bool{
		`.Post("/github-link/{task_uuid}", taskController.LinkTaskToGitHub)`:           true,
		`.Post("/github-unlink/{task_uuid}", taskController.UnlinkTaskFromGitHub)`:     true,
		`.Get("/github-activity/{task_uuid}", taskController.GetGitHubTaskActivity)`:   true,
		`.Get("/github-pr-reviews/{task_uuid}", taskController.GetGitHubPRReviews)`:    true,
		`.Get("/github-sync-status/{task_uuid}", taskController.GetGitHubSyncStatus)`:  true,
		`.Post("/github-refresh/{task_uuid}", taskController.RefreshFromGitHub)`:       true,
		`.Get("/github-search-issues/{task_uuid}", taskController.SearchGitHubIssues)`: true,
		`.Get("/github-search-prs/{task_uuid}", taskController.SearchGitHubPRs)`:       true,
		`.Post("/github-bulk-link", taskController.BulkLinkTasksToGitHub)`:             true,
		`.Post("/github-bulk-unlink", taskController.BulkUnlinkTasksFromGitHub)`:       true,
	}
	route := regexp.MustCompile(`\.(Get|Post|Put|Patch|Delete)\("[^"]*", taskController\.\w*(GitHub|Branch)\w*\)`)
	for _, found := range route.FindAllString(body, -1) {
		if !writes[found] && !reads[found] {
			t.Errorf("%s is a task route about GitHub that this test doesn't place: does it write to GitHub?", found)
		}
		if reads[found] && strings.Contains(body, "NoGitHubWritesInDemo)"+found) {
			t.Errorf("%s writes nothing to GitHub, but the demo refuses it", found)
		}
	}
}
