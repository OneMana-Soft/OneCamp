package business

// Links made before the guards (taskRepoLink.go): an issue address any member
// could store on any task, and pull requests that attached themselves to a
// task by naming it. Each start takes every task's issue or pull request in a
// repository its own project hasn't linked off the task, and logs each, so an
// admin can link it again where it belongs. Unlinking a repository from a
// project applies the same rule to that project's tasks at once
// (UnlinkRepository). Idempotent: an address taken off has nothing left to
// check, so later runs find nothing.

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	taskModels "github.com/akashc777/OneCamp/models/postgres/Task"
	"github.com/google/uuid"
)

// StartTaskLinkRepair runs RepairTaskLinks once, in the background.
func StartTaskLinkRepair(ctx context.Context) {
	helpers.GoSafeNamed("github.task-link-repair", func() {
		if _, err := RepairTaskLinks(ctx); err != nil {
			helpers.LogErrorWithContext(ctx, "github: could not check tasks' GitHub links against their projects' repositories, will at the next start: %v", err)
		}
	})
}

// RepairTaskLinks takes off, across the workspace, every task's issue or pull
// request outside its project's repositories, logs each, and returns them,
// each with the addresses taken off ("" for one kept). What can't be taken
// off now is tried again at each start, and the sync sends nothing of it
// meanwhile (syncAllowed).
func RepairTaskLinks(ctx context.Context) ([]taskModels.TaskGitHubLink, error) {
	return repairTaskLinks(ctx, uuid.Nil)
}

// repairTaskLinks is RepairTaskLinks for the tasks of project, or of every
// project when project is uuid.Nil: one read of the tasks linked to GitHub,
// one of the repositories their projects link, and one statement that takes
// off only what is outside, and only while each task still holds it.
func repairTaskLinks(ctx context.Context, project uuid.UUID) ([]taskModels.TaskGitHubLink, error) {
	rows, err := githubLinkModel.LinkedTasks(ctx, project)
	if err != nil {
		return nil, err
	}
	var links []*githubLinkModel.GitHubLink
	if project == uuid.Nil {
		links, err = GetAllLinkedRepos(ctx)
	} else {
		links, err = GetLinkedRepos(ctx, project)
	}
	if err != nil {
		return nil, err
	}
	unlinked, err := githubLinkModel.UnlinkTasks(ctx, outsideLinks(rows, links))
	if err != nil {
		return nil, err
	}
	for _, row := range unlinked {
		helpers.MessageLogs.InfoLog.Printf("github: unlinked task %s (project %s) from issue %q and pull request %q: a repository its project isn't linked to",
			row.TaskID, row.ProjectID, row.IssueURL, row.PRURL)
	}
	return unlinked, nil
}

// outsideLinks is each task whose issue or pull request isn't in a repository
// linked to its own project (an address that names no issue or pull request
// at all included), with those addresses alone: one inside is "", and stays
// on the task. Pure.
func outsideLinks(rows []taskModels.TaskGitHubLink, links []*githubLinkModel.GitHubLink) []taskModels.TaskGitHubLink {
	byProject := map[uuid.UUID][]*githubLinkModel.GitHubLink{}
	for _, l := range links {
		if l != nil {
			byProject[l.ProjectId] = append(byProject[l.ProjectId], l)
		}
	}
	outside := func(project uuid.UUID, address string) string {
		if address == "" {
			return ""
		}
		owner, repo, _, err := ExtractGitHubIssueInfo(address)
		if err == nil && repoLink(byProject[project], owner, repo) != nil {
			return ""
		}
		return address
	}
	var out []taskModels.TaskGitHubLink
	for _, r := range rows {
		issue, pr := outside(r.ProjectID, r.IssueURL), outside(r.ProjectID, r.PRURL)
		if issue != "" || pr != "" {
			out = append(out, taskModels.TaskGitHubLink{TaskID: r.TaskID, ProjectID: r.ProjectID, IssueURL: issue, PRURL: pr})
		}
	}
	return out
}
