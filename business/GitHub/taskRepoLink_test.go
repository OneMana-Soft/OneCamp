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

// No one project's link to a repository speaks for the others: what an event
// does without a task needs the first of the links that syncs it, and with
// none of them syncing it, the event does nothing.
func TestLinkSyncingIsTheFirstOfTheRepositorysLinksThatSyncsIt(t *testing.T) {
	quiet := &githubLinkModel.GitHubLink{Id: uuid.New(), AutoCreateTasks: true}
	issues := &githubLinkModel.GitHubLink{Id: uuid.New(), SyncIssues: true}
	both := &githubLinkModel.GitHubLink{Id: uuid.New(), SyncIssues: true, SyncPRs: true, AutoCreateTasks: true}
	links := []*githubLinkModel.GitHubLink{nil, quiet, issues, both}
	for name, c := range map[string]struct {
		syncs func(*githubLinkModel.GitHubLink) bool
		want  *githubLinkModel.GitHubLink
	}{
		"issues":                  {syncsIssues, issues},
		"pull requests":           {syncsPRs, both},
		"issues or pull requests": {syncsIssuesOrPRs, issues},
		"tasks of new issues":     {makesIssueTasks, both},
		"tasks of new pulls":      {makesPRTasks, both},
	} {
		if got := linkSyncing(links, c.syncs); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
	if got := linkSyncing([]*githubLinkModel.GitHubLink{nil, quiet}, syncsIssuesOrPRs); got != nil {
		t.Errorf("links that sync nothing gave %v", got)
	}
	if linkSyncing(nil, syncsIssues) != nil {
		t.Error("a repository nobody links has a link that syncs its issues")
	}
}

// The repair unlinks a task's issue or pull request in a repository its own
// project didn't link: one any member pointed it at, one another project's
// repository attached to it, or an address that names nothing. A task in its
// own project's repository stays linked, and so does the half of a task's
// link that is: an issue in the project's repository keeps its task when a
// pull request from elsewhere is taken off it.
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
	// Each task linked outside its project, with the issue and pull request
	// the repair takes off it ("" for one it keeps).
	unlinked := []struct {
		task      taskModels.TaskGitHubLink
		issue, pr string
	}{
		{task(pricing, "https://github.com/someone/elsewhere/issues/5", ""), "https://github.com/someone/elsewhere/issues/5", ""},
		{task(pricing, "", "https://github.com/acme/web/pull/6"), "", "https://github.com/acme/web/pull/6"},
		{task(product, "https://github.com/acme/web/issues/7", "https://github.com/acme/billing/pull/8"), "", "https://github.com/acme/billing/pull/8"},
		{task(pricing, "https://github.com/acme/web/issues/11", "https://github.com/acme/billing/pull/12"), "https://github.com/acme/web/issues/11", ""},
		{task(pricing, "https://github.com/someone/elsewhere/issues/13", "https://github.com/acme/web/pull/14"),
			"https://github.com/someone/elsewhere/issues/13", "https://github.com/acme/web/pull/14"},
		{task(uuid.Nil, "https://github.com/acme/web/issues/9", ""), "https://github.com/acme/web/issues/9", ""},
		{task(product, "https://example.com/acme/web/issues/10", ""), "https://example.com/acme/web/issues/10", ""},
	}
	all := append([]taskModels.TaskGitHubLink{}, kept...)
	for _, u := range unlinked {
		all = append(all, u.task)
	}
	got := outsideLinks(all, links)
	if len(got) != len(unlinked) {
		t.Fatalf("unlinks %d tasks, want %d: %+v", len(got), len(unlinked), got)
	}
	for i, u := range unlinked {
		want := taskModels.TaskGitHubLink{TaskID: u.task.TaskID, ProjectID: u.task.ProjectID, IssueURL: u.issue, PRURL: u.pr}
		if got[i] != want {
			t.Errorf("unlinks %+v, want %+v", got[i], want)
		}
	}
	if outsideLinks(kept, links) != nil {
		t.Error("a task linked inside its own project is unlinked")
	}
}
