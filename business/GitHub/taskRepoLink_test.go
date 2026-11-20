package business

import (
	"context"
	"errors"
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

// A task's stored branch is a ref like its key: it leads only to a task the
// integration's bot may read (botReads, as codeTask). A restricted task
// carrying the branch is no lead and hides no readable task carrying it too.
func TestABranchLeadsOnlyToATaskTheBotMayRead(t *testing.T) {
	oldTasks, oldReads := branchTasks, botReads
	t.Cleanup(func() { branchTasks, botReads = oldTasks, oldReads })

	restricted, made := uuid.New(), uuid.New()
	carriers := map[string][]string{
		"cache-sessions": {restricted.String(), made.String()},
		"private":        {restricted.String()},
		"garbled":        {"not-a-task", made.String()},
	}
	read := 0
	branchTasks = func(_ context.Context, owner, name, branch string) ([]string, error) {
		read++
		if branch == "gone" {
			return nil, errors.New("the read failed")
		}
		return carriers[branch], nil
	}
	botReads = func(_ context.Context, task uuid.UUID) bool { return task != restricted }

	for branch, want := range map[string]string{
		"cache-sessions": made.String(),
		"private":        "",
		"garbled":        made.String(),
		"gone":           "",
		"unknown":        "",
	} {
		if got := onBranch(context.Background(), "acme", "web", branch)(); got != want {
			t.Errorf("branch %q leads to %q, want %q", branch, got, want)
		}
	}
	read = 0
	if got := onBranch(context.Background(), "acme", "web", "")(); got != "" || read != 0 {
		t.Errorf("no branch leads to %q after %d reads, want none after none", got, read)
	}
}

// A sync of a task goes only where its own project links (CHAT76): through
// the project's online link it may send and read; through the project's
// offline link (the demo's fixture) there is nothing to send, and the sync is
// done; a repository another project links, online or offline, or that no
// project links, is outside its project. The offline read is made only when
// the online one finds nothing, and a read that fails says so.
func TestASyncReachesOnlyItsOwnProjectsRepositories(t *testing.T) {
	oldLinks, oldAll := readProjectLinks, readAllProjectLinks
	t.Cleanup(func() { readProjectLinks, readAllProjectLinks = oldLinks, oldAll })

	website, billing := uuid.New(), uuid.New()
	link := func(project uuid.UUID, owner, name string, online bool) *githubLinkModel.GitHubLink {
		return &githubLinkModel.GitHubLink{Id: uuid.New(), ProjectId: project, RepoOwner: owner, RepoName: name, SyncIssues: online, SyncPRs: online}
	}
	// Each project's links, offline ones included; only the online ones are
	// in readProjectLinks' answer, as githubLinkModel.Online leaves them.
	links := map[uuid.UUID][]*githubLinkModel.GitHubLink{
		website: {link(website, "acme", "web", true), link(website, "demo-team", "q4-launch-site", false)},
		billing: {link(billing, "acme", "billing", true), link(billing, "demo-team", "billing-site", false)},
	}
	var onlineReads, allReads int
	var onlineErr, allErr error
	readProjectLinks = func(_ context.Context, project uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
		onlineReads++
		var out []*githubLinkModel.GitHubLink
		for _, l := range links[project] {
			if l.SyncIssues || l.SyncPRs {
				out = append(out, l)
			}
		}
		return out, onlineErr
	}
	// As GetLinksForRepair reads: the project's links, or every project's
	// when the project is uuid.Nil.
	readAllProjectLinks = func(_ context.Context, project uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
		allReads++
		if project == uuid.Nil {
			return append(append([]*githubLinkModel.GitHubLink(nil), links[website]...), links[billing]...), allErr
		}
		return links[project], allErr
	}
	inWebsite := func(context.Context, uuid.UUID) (uuid.UUID, error) { return website, nil }
	inNoProject := func(context.Context, uuid.UUID) (uuid.UUID, error) { return uuid.Nil, nil }
	errRead := errors.New("the read failed")
	unread := func(context.Context, uuid.UUID) (uuid.UUID, error) { return uuid.Nil, errRead }

	task := uuid.New()
	for _, c := range []struct {
		name        string
		projectOf   func(context.Context, uuid.UUID) (uuid.UUID, error)
		owner, repo string
		online, all error
		want        error
		allRead     int // offline reads made
	}{
		{"its own project's online link", inWebsite, "acme", "web", nil, nil, nil, 0},
		{"its own project's online link, in another case", inWebsite, "Acme", "WEB", nil, nil, nil, 0},
		{"its own project's offline link", inWebsite, "demo-team", "q4-launch-site", nil, nil, errNothingToSend, 1},
		{"its own project's offline link, in another case", inWebsite, "Demo-Team", "Q4-Launch-Site", nil, nil, errNothingToSend, 1},
		{"another project's online link", inWebsite, "acme", "billing", nil, nil, errOutsideProject, 1},
		{"another project's offline link", inWebsite, "demo-team", "billing-site", nil, nil, errOutsideProject, 1},
		{"a repository no project links, by its project's offline one", inWebsite, "demo-team", "elsewhere", nil, nil, errOutsideProject, 1},
		{"its own project's offline link's owner with another name", inWebsite, "demo-team", "q4-launch", nil, nil, errOutsideProject, 1},
		{"a task in no project", inNoProject, "demo-team", "q4-launch-site", nil, nil, errOutsideProject, 0},
		{"its project can't be read", unread, "demo-team", "q4-launch-site", nil, nil, errRead, 0},
		{"its project's online links can't be read", inWebsite, "demo-team", "q4-launch-site", errRead, nil, errRead, 0},
		{"its project's offline links can't be read", inWebsite, "demo-team", "q4-launch-site", nil, errRead, errRead, 1},
	} {
		onlineReads, allReads, onlineErr, allErr = 0, 0, c.online, c.all
		got := syncReach(context.Background(), c.projectOf, task, c.owner, c.repo)
		if !errors.Is(got, c.want) {
			t.Errorf("%s: %s/%s answered %v; want %v", c.name, c.owner, c.repo, got, c.want)
		}
		if allReads != c.allRead {
			t.Errorf("%s: %d reads with offline links; want %d", c.name, allReads, c.allRead)
		}
	}
}
