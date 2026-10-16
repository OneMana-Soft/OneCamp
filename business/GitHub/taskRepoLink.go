package business

// A task's GitHub side belongs to its own project: the repositories linked to
// the task's project are the only ones its title, description, comments and
// status are sent to, and the only ones whose pull requests, branches and
// commits may attach to it or move it. Before, an issue address was stored as
// the client sent it, and a task id in a pull request's body or a branch's
// name (text whoever opens the pull request writes) was taken at its word, so
// a private project's discussion could be posted with the workspace's GitHub
// token to an issue or pull request the project never linked.

import (
	"context"
	"errors"
	"strings"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// errOutsideProject is a task's GitHub link to a repository its own project
// hasn't linked: nothing of the task goes there, and nothing from there comes
// back. The words are shown with the task's sync state.
var errOutsideProject = errors.New("this task's GitHub link is in a repository its project isn't linked to, so nothing was sent or read")

// TaskRepoLink is the link through which the task's own project reaches the
// repository owner/name, matched as GitHub matches names, in any case; nil
// when the task's project hasn't linked that repository, or the task is in no
// project (or gone).
func TaskRepoLink(ctx context.Context, taskID uuid.UUID, owner, name string) (*githubLinkModel.GitHubLink, error) {
	return projectRepoLink(ctx, taskDomain.GetTaskProjectID, taskID, owner, name)
}

// projectRepoLink is the link through which the task's project, as projectOf
// reads it, reaches owner/name; nil when it has none.
func projectRepoLink(ctx context.Context, projectOf func(context.Context, uuid.UUID) (uuid.UUID, error),
	taskID uuid.UUID, owner, name string) (*githubLinkModel.GitHubLink, error) {
	projectID, err := projectOf(ctx, taskID)
	if err != nil || projectID == uuid.Nil {
		return nil, err
	}
	links, err := GetLinkedRepos(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return repoLink(links, owner, name), nil
}

// What a repository's event is about, which the task's own project's link to
// the repository must sync for the event to touch the task.
var (
	syncsIssues      = func(l *githubLinkModel.GitHubLink) bool { return l.SyncIssues }
	syncsPRs         = func(l *githubLinkModel.GitHubLink) bool { return l.SyncPRs }
	syncsIssuesOrPRs = func(l *githubLinkModel.GitHubLink) bool { return l.SyncIssues || l.SyncPRs }
)

// eventTask is the task a repository's event names, and the link through
// which the event may change it: the task's own project's link to that
// repository, syncing what the event is about. That link, never another
// project's link to the same repository, decides what the event does and
// who is told. A task is named in text whoever opens a pull request or pushes
// a commit writes (a branch, a pull request's body, a commit message), so a
// task in a project that hasn't linked the repository, or doesn't sync that,
// isn't the event's: ok is false, and the event changes nothing on it.
func eventTask(ctx context.Context, taskUUIDStr, owner, name string, syncs func(*githubLinkModel.GitHubLink) bool) (uuid.UUID, *githubLinkModel.GitHubLink, bool) {
	taskID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		return uuid.Nil, nil, false
	}
	link, err := TaskRepoLink(ctx, taskID, owner, name)
	if err != nil || link == nil || !syncs(link) {
		helpers.MessageLogs.InfoLog.Printf("github: an event on %s/%s named task %s, which isn't in a project syncing that from the repository; left alone (err %v)",
			owner, name, taskID, err)
		return uuid.Nil, nil, false
	}
	return taskID, link, true
}

// repoLink is the link among links to the repository owner/name; nil if none.
// Pure.
func repoLink(links []*githubLinkModel.GitHubLink, owner, name string) *githubLinkModel.GitHubLink {
	for _, l := range links {
		if l != nil && strings.EqualFold(l.RepoOwner, owner) && strings.EqualFold(l.RepoName, name) {
			return l
		}
	}
	return nil
}

// syncAllowed is nil when owner/name is a repository the task's own project
// linked, so its sync may send there or read from there. Otherwise it marks
// the task's sync failed, saying why, and answers errOutsideProject, or the
// read's error when that can't be told.
//
// A deleted task's project is its project still: deleting a task sends its
// issue the task's last change, closing it as not planned. Read as live
// tasks are, a deleted one was refused as outside its project, and its
// issue stayed open.
func syncAllowed(ctx context.Context, taskID uuid.UUID, owner, name string) error {
	link, err := projectRepoLink(ctx, taskDomain.GetTaskProjectIDDeletedToo, taskID, owner, name)
	if err == nil && link != nil {
		return nil
	}
	reason := "couldn't check the task's project"
	if err == nil {
		err, reason = errOutsideProject, errOutsideProject.Error()
	}
	_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "failed", &reason, 1)
	return err
}
