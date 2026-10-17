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
	"sync"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// errOutsideProject is a task's GitHub link to a repository its own project
// hasn't linked: nothing of the task goes there, and nothing from there comes
// back. The words are shown with the task's sync state.
var errOutsideProject = errors.New("this task's GitHub link is in a repository its project isn't linked to, so nothing was sent or read")

// The two reads that tell which link reaches a task: the project the task is
// in, and the repositories that project links. Package variables, so a test
// can count them.
var (
	readTaskProject  = taskDomain.GetTaskProjectID
	readProjectLinks = GetLinkedRepos
)

// eventReads is what one GitHub event has read of the tasks it names: the
// project each is in, and the repositories each of those projects links. A
// push lists up to 2,048 commits and a task's automation rule checks its link
// again, so without it a task named in many commits was read once per commit.
// HandleGitHubWebhookEvent starts one per event (withEventReads); outside an
// event, every check reads afresh.
type eventReads struct {
	mu       sync.Mutex
	projects map[uuid.UUID]uuid.UUID
	links    map[uuid.UUID][]*githubLinkModel.GitHubLink
}

type eventReadsKey struct{}

// withEventReads starts an event's reads.
func withEventReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, eventReadsKey{}, &eventReads{
		projects: map[uuid.UUID]uuid.UUID{},
		links:    map[uuid.UUID][]*githubLinkModel.GitHubLink{},
	})
}

// taskProject is the project the task is in, read once in an event.
func taskProject(ctx context.Context, taskID uuid.UUID) (uuid.UUID, error) {
	r, _ := ctx.Value(eventReadsKey{}).(*eventReads)
	if r == nil {
		return readTaskProject(ctx, taskID)
	}
	return remembered(&r.mu, r.projects, taskID, func() (uuid.UUID, error) { return readTaskProject(ctx, taskID) })
}

// projectLinks is the repositories the project links, read once in an event.
func projectLinks(ctx context.Context, projectID uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
	r, _ := ctx.Value(eventReadsKey{}).(*eventReads)
	if r == nil {
		return readProjectLinks(ctx, projectID)
	}
	return remembered(&r.mu, r.links, projectID, func() ([]*githubLinkModel.GitHubLink, error) { return readProjectLinks(ctx, projectID) })
}

// remembered is seen[key], read by read the first time it is asked for and
// kept once a read succeeds (a failed one is tried again).
func remembered[K comparable, V any](mu *sync.Mutex, seen map[K]V, key K, read func() (V, error)) (V, error) {
	mu.Lock()
	v, ok := seen[key]
	mu.Unlock()
	if ok {
		return v, nil
	}
	v, err := read()
	if err != nil {
		return v, err
	}
	mu.Lock()
	seen[key] = v
	mu.Unlock()
	return v, nil
}

// TaskRepoLink is the link through which the task's own project reaches the
// repository owner/name, matched as GitHub matches names, in any case; nil
// when the task's project hasn't linked that repository, or the task is in no
// project (or gone).
func TaskRepoLink(ctx context.Context, taskID uuid.UUID, owner, name string) (*githubLinkModel.GitHubLink, error) {
	return projectRepoLink(ctx, taskProject, taskID, owner, name)
}

// TaskRepo asks whether the task's own project links the repository
// Owner/Name, as TaskRepoLink does for one task.
type TaskRepo struct {
	Task        uuid.UUID
	Owner, Name string
}

// readTaskRepoLinks is the one read behind TaskRepoLinks; a package variable,
// so a test can count it.
var readTaskRepoLinks = githubLinkModel.TasksLinkingRepos

// TaskRepoLinks answers TaskRepoLink's question for many tasks in one read:
// which of asks name a repository their task's own project links, matched in
// any case. A bulk change asks it once, where asking per task cost two reads
// apiece.
func TaskRepoLinks(ctx context.Context, asks []TaskRepo) (map[TaskRepo]bool, error) {
	out := make(map[TaskRepo]bool, len(asks))
	if len(asks) == 0 {
		return out, nil
	}
	tasks, owners, names := make([]uuid.UUID, len(asks)), make([]string, len(asks)), make([]string, len(asks))
	for i, ask := range asks {
		tasks[i], owners[i], names[i] = ask.Task, ask.Owner, ask.Name
	}
	linked, err := readTaskRepoLinks(ctx, tasks, owners, names)
	if err != nil {
		return nil, err
	}
	for _, i := range linked {
		out[asks[i]] = true
	}
	return out, nil
}

// projectRepoLink is the link through which the task's project, as projectOf
// reads it, reaches owner/name; nil when it has none.
func projectRepoLink(ctx context.Context, projectOf func(context.Context, uuid.UUID) (uuid.UUID, error),
	taskID uuid.UUID, owner, name string) (*githubLinkModel.GitHubLink, error) {
	projectID, err := projectOf(ctx, taskID)
	if err != nil || projectID == uuid.Nil {
		return nil, err
	}
	links, err := projectLinks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return repoLink(links, owner, name), nil
}

// What a repository's event is about, which the task's own project's link to
// the repository must sync for the event to touch the task, and some
// project's link must sync for the event to do anything without one.
var (
	syncsIssues      = func(l *githubLinkModel.GitHubLink) bool { return l.SyncIssues }
	syncsPRs         = func(l *githubLinkModel.GitHubLink) bool { return l.SyncPRs }
	syncsIssuesOrPRs = func(l *githubLinkModel.GitHubLink) bool { return l.SyncIssues || l.SyncPRs }
	// A link that makes a task of each new issue, or pull request, it syncs.
	makesIssueTasks = func(l *githubLinkModel.GitHubLink) bool { return l.SyncIssues && l.AutoCreateTasks }
	makesPRTasks    = func(l *githubLinkModel.GitHubLink) bool { return l.SyncPRs && l.AutoCreateTasks }
)

// linkSyncing is the first of a repository's links (repositoryLinks, the
// oldest first) that does what syncs asks; nil when none does. With none, an
// event of that kind has nothing to do: a task's own project's link is one of
// them (eventTask), and so is any link that acts without a task. Pure.
func linkSyncing(links []*githubLinkModel.GitHubLink, syncs func(*githubLinkModel.GitHubLink) bool) *githubLinkModel.GitHubLink {
	for _, l := range links {
		if l != nil && syncs(l) {
			return l
		}
	}
	return nil
}

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

// leadTask is the first task, of those the leads name in turn, that an event
// on owner/name may change (eventTask), with its own project's link; ok is
// false when none is. A lead (a task's id, "" for none) is read only when
// those before it named no such task: what names a task (a branch, a pull
// request's body) is whoever opened it's to write, so the task one names may
// be refused where the next names the event's own.
func leadTask(ctx context.Context, owner, name string, syncs func(*githubLinkModel.GitHubLink) bool,
	leads ...func() string) (string, uuid.UUID, *githubLinkModel.GitHubLink, bool) {
	for _, lead := range leads {
		taskUUIDStr := lead()
		if taskUUIDStr == "" {
			continue
		}
		if taskID, link, ok := eventTask(ctx, taskUUIDStr, owner, name, syncs); ok {
			return taskUUIDStr, taskID, link, true
		}
	}
	return "", uuid.Nil, nil, false
}

// onBranch is a lead to the task on branch of a project that links owner/name
// ("" for none, and for a read that failed, which the read logs).
func onBranch(ctx context.Context, owner, name, branch string) func() string {
	return func() string {
		if branch == "" {
			return ""
		}
		taskUUIDStr, _ := taskDomain.FindTaskUUIDByGitHubBranch(ctx, owner, name, branch)
		return taskUUIDStr
	}
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
