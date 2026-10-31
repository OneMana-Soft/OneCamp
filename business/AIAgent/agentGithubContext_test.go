package business

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
)

// A run is told only of the repositories its code tools would read: each is
// judged on every project linking it, as CodeRepoAllowed judges it, so a
// repository linked to a project outside the run's reach is never named (A13).
func TestARunIsToldOnlyOfRepositoriesItMayRead(t *testing.T) {
	web, docs, billing := uuid.New(), uuid.New(), uuid.New()
	link := func(owner, name string, project uuid.UUID) *githubLinkModel.GitHubLink {
		return &githubLinkModel.GitHubLink{RepoOwner: owner, RepoName: name, ProjectId: project}
	}
	links := []*githubLinkModel.GitHubLink{
		link("acme", "web", web),
		link("acme", "billing", billing),
		nil,
		link("ACME", "Web", docs),
		link("acme", " ", web),
	}
	var asked [][]uuid.UUID
	got := readableRepos(links, func(projects []uuid.UUID) bool {
		asked = append(asked, projects)
		for _, p := range projects {
			if p == billing {
				return false
			}
		}
		return true
	})
	if !reflect.DeepEqual(got, []string{"acme/web"}) {
		t.Errorf("listed %v, want acme/web alone, spelled as its first link given", got)
	}
	if want := [][]uuid.UUID{{web, docs}, {billing}}; !reflect.DeepEqual(asked, want) {
		t.Errorf("asked about %v, want each repository once, with every project linking it", asked)
	}
}

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
