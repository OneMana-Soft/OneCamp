package business

// Links made before the guards (taskRepoLink.go): an issue address any member
// could store on any task, and pull requests that attached themselves to a
// task by naming it. Each start unlinks every task whose stored issue or pull
// request is in a repository its own project hasn't linked, and logs each
// one, so an admin can link it again where it belongs. Unlinking a repository
// from a project applies the same rule at once (UnlinkRepository).
// Idempotent: an unlinked task has nothing left to check, so later runs find
// nothing.

import (
	"context"
	"fmt"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
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

// RepairTaskLinks unlinks every task linked to an issue or pull request
// outside its project's repositories, logs each, and returns them. Any it
// couldn't unlink are counted in the error (each start tries again, and the
// sync sends nothing of theirs meanwhile).
func RepairTaskLinks(ctx context.Context) ([]taskModels.TaskGitHubLink, error) {
	rows, err := taskDomain.GetTaskGitHubLinks(ctx)
	if err != nil {
		return nil, err
	}
	links, err := GetAllLinkedRepos(ctx)
	if err != nil {
		return nil, err
	}
	var unlinked []taskModels.TaskGitHubLink
	failed := 0
	for _, row := range outsideLinks(rows, links) {
		if err := taskDomain.ClearGitHubLinkFromTask(ctx, row.TaskID); err != nil {
			failed++
			continue
		}
		helpers.MessageLogs.InfoLog.Printf("github: unlinked task %s (project %s) from issue %q and pull request %q: a repository its project isn't linked to",
			row.TaskID, row.ProjectID, row.IssueURL, row.PRURL)
		unlinked = append(unlinked, row)
	}
	if failed > 0 {
		return unlinked, fmt.Errorf("%d of %d tasks linked outside their project's repositories couldn't be unlinked", failed, failed+len(unlinked))
	}
	return unlinked, nil
}

// outsideLinks is the tasks whose issue or pull request isn't in a repository
// linked to their own project, including an address that names no issue or
// pull request at all. Pure.
func outsideLinks(rows []taskModels.TaskGitHubLink, links []*githubLinkModel.GitHubLink) []taskModels.TaskGitHubLink {
	byProject := map[uuid.UUID][]*githubLinkModel.GitHubLink{}
	for _, l := range links {
		if l != nil {
			byProject[l.ProjectId] = append(byProject[l.ProjectId], l)
		}
	}
	inside := func(project uuid.UUID, address string) bool {
		if address == "" {
			return true
		}
		owner, repo, _, err := ExtractGitHubIssueInfo(address)
		return err == nil && repoLink(byProject[project], owner, repo) != nil
	}
	var out []taskModels.TaskGitHubLink
	for _, r := range rows {
		if !inside(r.ProjectID, r.IssueURL) || !inside(r.ProjectID, r.PRURL) {
			out = append(out, r)
		}
	}
	return out
}
