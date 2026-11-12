package business

import (
	"context"
	"io"
	"log"
	"testing"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	"github.com/akashc777/OneCamp/helpers"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// R1: a key in code resolves and moves only a task in a project linked to the
// repository (its own project's link, syncing pull requests), and never one
// the integration's bot can't read. A key from another project, or one that
// names nothing, does nothing. Each key is read once in an event.
func TestCodeNamesOnlyTheRepositorysReadableTasks(t *testing.T) {
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
	oldProject, oldLinks, oldKey, oldReads := readTaskProject, readProjectLinks, keyTask, botReads
	t.Cleanup(func() { readTaskProject, readProjectLinks, keyTask, botReads = oldProject, oldLinks, oldKey, oldReads })

	website, billing := uuid.New(), uuid.New()
	mine, theirs, private := uuid.New(), uuid.New(), uuid.New()
	projects := map[uuid.UUID]uuid.UUID{mine: website, theirs: billing, private: website}
	keys := map[string]uuid.UUID{"WEB-1": mine, "BIL-1": theirs, "WEB-2": private}
	readTaskProject = func(_ context.Context, task uuid.UUID) (uuid.UUID, error) { return projects[task], nil }
	readProjectLinks = func(_ context.Context, project uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
		repo := map[uuid.UUID]string{website: "web", billing: "billing"}[project]
		return []*githubLinkModel.GitHubLink{{ProjectId: project, RepoOwner: "acme", RepoName: repo, SyncPRs: true}}, nil
	}
	reads := map[string]int{}
	keyTask = func(_ context.Context, raw string) (uuid.UUID, error) {
		reads[raw]++
		if id, ok := keys[raw]; ok {
			return id, nil
		}
		return uuid.Nil, taskKeys.ErrNoTask
	}
	botReads = func(_ context.Context, task uuid.UUID) bool { return task != private }

	ctx := withEventReads(context.Background())
	named := func(ref taskKeys.CodeRef) (uuid.UUID, bool) {
		id, link, ok := codeTask(ctx, ref, "acme", "web")
		if ok && (link == nil || link.ProjectId != website) {
			t.Errorf("%+v came through %+v, want Website's own link", ref, link)
		}
		return id, ok
	}
	if id, ok := named(taskKeys.CodeRef{Key: "WEB-1", Close: true}); !ok || id != mine {
		t.Errorf("WEB-1 on Website's repository: %s %v", id, ok)
	}
	if id, ok := named(taskKeys.CodeRef{ID: mine}); !ok || id != mine {
		t.Errorf("task/<id> on Website's repository: %s %v", id, ok)
	}
	for _, ref := range []taskKeys.CodeRef{
		{Key: "BIL-1", Close: true}, // another project's key
		{ID: theirs, Close: true},   // another project's task by its address
		{Key: "NOPE-9", Close: true},
		{Key: "WEB-2", Close: true}, // a task the bot can't read
	} {
		if id, ok := named(ref); ok {
			t.Errorf("%+v named %s on acme/web", ref, id)
		}
	}
	for i := 0; i < 50; i++ {
		named(taskKeys.CodeRef{Key: "WEB-1"})
		named(taskKeys.CodeRef{Key: "NOPE-9"})
	}
	if reads["WEB-1"] != 1 || reads["NOPE-9"] != 1 {
		t.Errorf("keys read %v, want each once in the event", reads)
	}
	// The same refs on Billing's repository name only Billing's task.
	if _, _, ok := codeTask(ctx, taskKeys.CodeRef{Key: "WEB-1"}, "acme", "billing"); ok {
		t.Error("Website's task was named on Billing's repository")
	}
	if id, _, ok := codeTask(ctx, taskKeys.CodeRef{Key: "BIL-1"}, "acme", "billing"); !ok || id != theirs {
		t.Errorf("BIL-1 on Billing's repository: %s %v", id, ok)
	}
}

// A closing ref moves its task to the link's "Commit linked" status, to done
// when the link sets none, and nowhere when the rule says no change.
func TestCloseTarget(t *testing.T) {
	cases := []struct {
		rules map[string]string
		want  string
	}{
		{nil, "done"},
		{map[string]string{"pr_merged": "inReview"}, "done"},
		{map[string]string{"commit_linked": "inReview"}, "inReview"},
		{map[string]string{"commit_linked": " qa-id "}, "qa-id"},
		{map[string]string{"commit_linked": "_none"}, ""},
	}
	for _, c := range cases {
		if got := closeTarget(c.rules); got != c.want {
			t.Errorf("closeTarget(%v) = %q, want %q", c.rules, got, c.want)
		}
	}
}
