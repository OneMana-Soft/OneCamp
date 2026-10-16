package business

import (
	"testing"

	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	taskModels "github.com/akashc777/OneCamp/models/postgres/Task"
	"github.com/google/uuid"
)

// A task's repository is one its own project linked, matched as GitHub
// matches names: any case, but the whole owner and the whole name.
func TestRepoLinkIsTheProjectsOwnLinkToThatRepository(t *testing.T) {
	web := &githubLinkModel.GitHubLink{Id: uuid.New(), RepoOwner: "Acme", RepoName: "Web"}
	links := []*githubLinkModel.GitHubLink{nil, {Id: uuid.New(), RepoOwner: "acme", RepoName: "web-old"}, web}
	if got := repoLink(links, "acme", "web"); got != web {
		t.Errorf("acme/web found %v, want the project's link", got)
	}
	for _, other := range [][2]string{{"acme", "we"}, {"evil", "web"}, {"acme", "sdk"}, {"", ""}} {
		if got := repoLink(links, other[0], other[1]); got != nil {
			t.Errorf("%s/%s found the link to %s/%s", other[0], other[1], got.RepoOwner, got.RepoName)
		}
	}
	if repoLink(nil, "acme", "web") != nil {
		t.Error("a project with no links reaches a repository")
	}
}

// The repair unlinks a task whose issue or pull request is in a repository its
// own project didn't link: one any member pointed it at, one another
// project's repository attached to it, or an address that names nothing. A task
// in its own project's repository stays linked.
func TestTheRepairUnlinksOnlyLinksOutsideTheTasksProject(t *testing.T) {
	product, pricing := uuid.New(), uuid.New()
	links := []*githubLinkModel.GitHubLink{
		{ProjectId: product, RepoOwner: "acme", RepoName: "web"},
		{ProjectId: pricing, RepoOwner: "acme", RepoName: "billing"},
	}
	task := func(project uuid.UUID, issue, pr string) taskModels.TaskGitHubLink {
		return taskModels.TaskGitHubLink{TaskID: uuid.New(), ProjectID: project, IssueURL: issue, PRURL: pr}
	}
	kept := []taskModels.TaskGitHubLink{
		task(product, "https://github.com/acme/web/issues/1", ""),
		task(product, "", "https://github.com/Acme/Web/pull/2"),
		task(pricing, "https://github.com/acme/billing/issues/3", "https://github.com/acme/billing/pull/4"),
	}
	unlinked := []taskModels.TaskGitHubLink{
		task(pricing, "https://github.com/someone/elsewhere/issues/5", ""),
		task(pricing, "", "https://github.com/acme/web/pull/6"),
		task(product, "https://github.com/acme/web/issues/7", "https://github.com/acme/billing/pull/8"),
		task(uuid.Nil, "https://github.com/acme/web/issues/9", ""),
		task(product, "https://example.com/acme/web/issues/10", ""),
	}
	got := outsideLinks(append(append([]taskModels.TaskGitHubLink{}, kept...), unlinked...), links)
	if len(got) != len(unlinked) {
		t.Fatalf("unlinks %d tasks, want %d: %+v", len(got), len(unlinked), got)
	}
	for i := range unlinked {
		if got[i].TaskID != unlinked[i].TaskID {
			t.Errorf("unlinks %+v, want %+v", got[i], unlinked[i])
		}
	}
	if outsideLinks(kept, links) != nil {
		t.Error("a task linked inside its own project is unlinked")
	}
}
