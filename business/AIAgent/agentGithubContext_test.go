package business

import (
	"strings"
	"testing"
)

func TestRenderGitHubContext_Empty(t *testing.T) {
	if got := renderGitHubContext(nil); got != "" {
		t.Fatalf("expected empty string for no repos, got %q", got)
	}
	if got := renderGitHubContext([]string{}); got != "" {
		t.Fatalf("expected empty string for empty slice, got %q", got)
	}
}

func TestRenderGitHubContext_ListsReposAndRules(t *testing.T) {
	repos := []string{"akashc777/OneCamp-fe", "acme/api"}
	got := renderGitHubContext(repos)

	for _, r := range repos {
		if !strings.Contains(got, r) {
			t.Errorf("expected output to list repo %q, got:\n%s", r, got)
		}
	}

	// Private-repo grounding: must steer away from search and toward direct access.
	mustContain := []string{
		"PRIVATE",
		"search",       // must mention not using search
		"list_commits", // must name the deterministic direct-commits tool
		"needs_human",
	}
	lower := strings.ToLower(got)
	for _, sub := range mustContain {
		if !strings.Contains(lower, strings.ToLower(sub)) {
			t.Errorf("expected github rules to mention %q, got:\n%s", sub, got)
		}
	}
}

func TestRenderGitHubContext_DisambiguationDirective(t *testing.T) {
	got := renderGitHubContext([]string{"a/one", "b/two"})
	lower := strings.ToLower(got)
	// Must instruct asking the user when the match is ambiguous.
	if !strings.Contains(lower, "more than one") {
		t.Errorf("expected disambiguation directive for multiple matches, got:\n%s", got)
	}
}
