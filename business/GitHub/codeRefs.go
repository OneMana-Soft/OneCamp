package business

// Code names tasks (P10-1a). A commit message, a pull request's title or body,
// or a branch's name names a task by its key or its address, and a closing
// word ("Fixes WEB-12") says the change closes it (business/TaskKeys RefsIn,
// the one reading of it). Those are the words of whoever pushes or opens the
// pull request, so a ref changes a task only:
//
//   - in a project that linked the repository, through that project's own
//     link syncing pull requests (eventTask); a key from another project, or
//     one that names nothing, does nothing;
//   - that the integration's bot may read: a task restricted to named people
//     is theirs alone;
//   - as the integration's bot (GitHub), never as a person, in the task's
//     history.
//
// A closing ref moves its task to the link's "Commit linked" status, done when
// the link sets none, once its commit lands on the repository's default branch
// or its pull request merges.

import (
	"context"
	"errors"
	"strings"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	githubTaskActivityDomain "github.com/akashc777/OneCamp/domain/GitHubTaskActivity"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// provider is this package's code host, as its integration's bot is named.
const provider = "github"

// The trigger a closing ref fires: the card calls it "Commit linked".
const closeTrigger = "commit_linked"

// Seams for tests: the task a key names, whether the integration's bot may
// read a task, and the bot.
var (
	keyTask        = taskKeys.Resolve
	botReads       = integrationBotReads
	integrationBot = func(ctx context.Context) (*dgraphStruct.DgraphUser, error) {
		b, err := userBusiness.EnsureIntegrationBot(ctx, provider, "GitHub")
		if err != nil {
			return nil, err
		}
		return &dgraphStruct.DgraphUser{Uid: b.DgraphUID, Uuid: b.UUID, UserName: b.Name,
			EmailID: userDomain.IntegrationBotEmail(provider), IsBot: true}, nil
	}
)

// codeTask is the task ref names and its own project's link to owner/name,
// when an event on that repository may change it (see the top of the file);
// ok is false otherwise, and nothing about the ref is logged but that.
func codeTask(ctx context.Context, ref taskKeys.CodeRef, owner, name string) (uuid.UUID, *githubLinkModel.GitHubLink, bool) {
	taskID := ref.ID
	if ref.Key != "" {
		taskID = keyedTask(ctx, ref.Key)
	}
	if taskID == uuid.Nil {
		return uuid.Nil, nil, false
	}
	id, link, ok := eventTask(ctx, taskID.String(), owner, name, syncsPRs)
	if !ok || !botReads(ctx, id) {
		return uuid.Nil, nil, false
	}
	return id, link, true
}

// keyedTask is the task key names, read once in an event; uuid.Nil for none,
// and for a read that failed (logged).
func keyedTask(ctx context.Context, key string) uuid.UUID {
	read := func() (uuid.UUID, error) {
		id, err := keyTask(ctx, key)
		if errors.Is(err, taskKeys.ErrNoTask) {
			return uuid.Nil, nil
		}
		return id, err
	}
	var id uuid.UUID
	var err error
	if r, _ := ctx.Value(eventReadsKey{}).(*eventReads); r != nil {
		id, err = remembered(&r.mu, r.keys, key, read)
	} else {
		id, err = read()
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "github: couldn't read which task a key in code names: %v", err)
		return uuid.Nil
	}
	return id
}

// namedIn is a lead to the first task refs name that an event on owner/name
// may change (codeTask); "" for none.
func namedIn(ctx context.Context, owner, name string, refs []taskKeys.CodeRef) func() string {
	return func() string {
		for _, ref := range refs {
			if id, _, ok := codeTask(ctx, ref, owner, name); ok {
				return id.String()
			}
		}
		return ""
	}
}

// integrationBotReads is whether the integration's bot may read the task: not
// one restricted to people it isn't one of. A read that fails reads as no.
func integrationBotReads(ctx context.Context, task uuid.UUID) bool {
	bot, err := integrationBot(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "github: couldn't resolve the integration's bot: %v", err)
		return false
	}
	facts, err := workAccess.FactsFor(ctx, bot.Uuid, bot.Uid, &dgraphStruct.DgraphTask{Uuid: task.String()})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "github: couldn't read whether task %s is restricted: %v", task, err)
		return false
	}
	return !facts.Restricted || facts.Granted
}

// closeTarget is the status a closing ref moves its task to: the link's
// "Commit linked" rule, done when it sets none, "" when it says no change.
// Pure.
func closeTarget(rules map[string]string) string {
	if strings.TrimSpace(rules[closeTrigger]) == "_none" {
		return ""
	}
	if v := ruleTarget(rules, closeTrigger); v != "" {
		return v
	}
	return dgraphStruct.TASK_STATUS_DONE
}

// closeByRef moves a task a closing ref names (codeTask let it through, by
// its own project's link) as the integration's bot, as every rule does.
func closeByRef(ctx context.Context, link *githubLinkModel.GitHubLink, taskID uuid.UUID) {
	moveByRule(ctx, link, taskID.String(), closeTrigger,
		func(l *githubLinkModel.GitHubLink) string { return closeTarget(parseAutomationRules(l)) })
}

// closeMergedRefs moves each task a merged pull request's title or body says
// it fixes or closes (refs) that the event may change. A task other than the
// pull request's own (lead) takes a line saying it merged, as its own does.
func closeMergedRefs(ctx context.Context, owner, name string, refs []taskKeys.CodeRef, lead uuid.UUID, login, title string) {
	closed := map[uuid.UUID]bool{}
	for _, ref := range refs {
		if !ref.Close {
			continue
		}
		taskID, own, ok := codeTask(ctx, ref, owner, name)
		if !ok || closed[taskID] {
			continue
		}
		closed[taskID] = true
		if taskID != lead {
			_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskID, "pr_merged", &login, nil, nil, &title, nil, nil)
			publishGitHubSyncMqtt(taskID.String(), own.ProjectId.String(), "pr_merged", nil)
		}
		closeByRef(ctx, own, taskID)
	}
}
