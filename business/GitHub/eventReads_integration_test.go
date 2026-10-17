//go:build integration

package business

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A push lists up to 2,048 commits, and each commit naming a task cost the
// task's two reads (its project, and the repositories that project links)
// again, and the task's automation rule read them once more. One event now
// reads each task's project, and each project's repositories, once; the next
// event reads them afresh.
func TestAnEventReadsEachTaskItNamesOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)

	// Website links acme/reads and holds two tasks; Billing doesn't link it.
	user, website, billing := uuid.New(), uuid.New(), uuid.New()
	fix, docs, invoices := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id) VALUES ($1, $2)`, []any{user, user.String()[:8] + "@example.test"}},
		{`INSERT INTO projects (id, project_name) VALUES ($1, 'Website'), ($2, 'Billing')`, []any{website, billing}},
		{`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4)`, []any{fix, docs, website, user}},
		{`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, []any{invoices, billing, user}},
		{`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, automation_rules, created_by)
			VALUES ($1, 'acme', 'reads', true, true, '{}'::jsonb, $2)`, []any{website, user}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seed %q: %v", q.sql, err)
		}
	}

	var mu sync.Mutex
	projectReads, linkReads := map[uuid.UUID]int{}, map[uuid.UUID]int{}
	realProject, realLinks := readTaskProject, readProjectLinks
	t.Cleanup(func() { readTaskProject, readProjectLinks = realProject, realLinks })
	readTaskProject = func(ctx context.Context, task uuid.UUID) (uuid.UUID, error) {
		mu.Lock()
		projectReads[task]++
		mu.Unlock()
		return realProject(ctx, task)
	}
	readProjectLinks = func(ctx context.Context, project uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
		mu.Lock()
		linkReads[project]++
		mu.Unlock()
		return realLinks(ctx, project)
	}
	reads := func() (projects, links map[uuid.UUID]int) {
		mu.Lock()
		defer mu.Unlock()
		projects, links = map[uuid.UUID]int{}, map[uuid.UUID]int{}
		for k, v := range projectReads {
			projects[k] = v
		}
		for k, v := range linkReads {
			links[k] = v
		}
		return projects, links
	}
	event := func(kind string, payload map[string]any) {
		t.Helper()
		payload["repository"] = map[string]any{"full_name": "acme/reads", "name": "reads", "owner": map[string]any{"login": "acme"}}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := HandleGitHubWebhookEvent(ctx, kind, body); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}

	// history is how many entries the tasks' GitHub history has, by kind.
	history := func() map[string]int {
		t.Helper()
		rows, err := env.PG.Query(`SELECT activity_type, COUNT(*) FROM github_task_activities WHERE task_id IN ($1, $2, $3) GROUP BY activity_type`,
			fix, docs, invoices)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var kind string
			var n int
			if err := rows.Scan(&kind, &n); err != nil {
				t.Fatal(err)
			}
			out[kind] = n
		}
		return out
	}

	// One push: four hundred commits name the fix, three the docs task, and
	// five a Billing task, which the push may not change.
	var commits []map[string]any
	for task, n := range map[uuid.UUID]int{fix: 400, docs: 3, invoices: 5} {
		for i := 0; i < n; i++ {
			sha := fmt.Sprintf("%s%04d", task.String()[:8], i)
			commits = append(commits, map[string]any{"id": sha, "message": "Touches task/" + task.String(),
				"url": "https://github.com/acme/reads/commit/" + sha})
		}
	}
	event("push", map[string]any{"ref": "refs/heads/main", "sender": map[string]any{"login": "stranger"}, "commits": commits})
	// The commits are in the tasks' history once the push is handled: the
	// delivery is recorded as done then. Each was written on a goroutine of
	// its own, one per commit, after it.
	if got := history(); got["commit_linked"] != 403 || len(got) != 1 {
		t.Errorf("once the push was handled the tasks' history held %v, want the 403 commits naming the Website tasks", got)
	}
	projects, links := reads()
	if projects[fix] != 1 || projects[docs] != 1 || projects[invoices] != 1 {
		t.Errorf("one push read the fix's project %d times, the docs task's %d and the Billing task's %d, want once each",
			projects[fix], projects[docs], projects[invoices])
	}
	if links[website] != 1 || links[billing] != 1 {
		t.Errorf("one push read Website's repositories %d times and Billing's %d, want once each", links[website], links[billing])
	}

	// The next event reads afresh, once, though the pull request's rule checks
	// the fix's link again.
	event("pull_request", map[string]any{"action": "opened", "pull_request": map[string]any{"number": 3, "title": "Fix it",
		"html_url": "https://github.com/acme/reads/pull/3", "state": "open", "body": "Closes https://team.example.test/app/task/" + fix.String(),
		"user": map[string]any{"login": "stranger"}, "head": map[string]any{"ref": "fix-it"}, "updated_at": time.Now().UTC().Format(time.RFC3339)}})
	after, afterLinks := reads()
	if after[fix]-projects[fix] != 1 || afterLinks[website]-links[website] != 1 {
		t.Errorf("a pull request read the fix's project %d times and Website's repositories %d, want once each",
			after[fix]-projects[fix], afterLinks[website]-links[website])
	}
	if got := history(); got["pr_opened"] != 1 {
		t.Errorf("once the pull request was handled the tasks' history held %v, want it opened", got)
	}
}

// A bulk link checks each task's issue against the repositories of the task's
// own project. Asked per task, that was two reads apiece, up to a hundred
// tasks; asked for all of them, it is one.
func TestManyTasksAreCheckedAgainstTheirRepositoriesInOneRead(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}

	// Website links acme/web and Acme/Docs; Billing links nothing.
	user, website, billing := uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, user, user.String()[:8]+"@example.test")
	exec(`INSERT INTO projects (id, project_name) VALUES ($1, 'Website'), ($2, 'Billing')`, website, billing)
	exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, created_by) VALUES ($1, 'acme', 'web', $2), ($1, 'Acme', 'Docs', $2)`, website, user)
	task := func(project uuid.UUID, deleted bool) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO tasks (id, project_id, created_by, deleted_at) VALUES ($1, $2, $3, CASE WHEN $4 THEN NOW() END)`, id, project, user, deleted)
		return id
	}

	var asks []TaskRepo
	want := map[TaskRepo]bool{}
	for i := 0; i < 30; i++ {
		ask := TaskRepo{Task: task(website, false), Owner: "acme", Name: "web"}
		asks, want[ask] = append(asks, ask), true
	}
	docs := TaskRepo{Task: task(website, false), Owner: "ACME", Name: "docs"}
	asks, want[docs] = append(asks, docs), true
	asks = append(asks,
		TaskRepo{Task: task(billing, false), Owner: "acme", Name: "web"},
		TaskRepo{Task: task(website, true), Owner: "acme", Name: "web"},
		TaskRepo{Task: uuid.New(), Owner: "acme", Name: "web"},
		TaskRepo{Task: asks[0].Task, Owner: "acme", Name: "elsewhere"})

	var mu sync.Mutex
	perTask, batched := 0, 0
	realProject, realLinks, realBatch := readTaskProject, readProjectLinks, readTaskRepoLinks
	t.Cleanup(func() { readTaskProject, readProjectLinks, readTaskRepoLinks = realProject, realLinks, realBatch })
	readTaskProject = func(ctx context.Context, task uuid.UUID) (uuid.UUID, error) {
		mu.Lock()
		perTask++
		mu.Unlock()
		return realProject(ctx, task)
	}
	readProjectLinks = func(ctx context.Context, project uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
		mu.Lock()
		perTask++
		mu.Unlock()
		return realLinks(ctx, project)
	}
	readTaskRepoLinks = func(ctx context.Context, tasks []uuid.UUID, owners, names []string) ([]int, error) {
		mu.Lock()
		batched++
		mu.Unlock()
		return realBatch(ctx, tasks, owners, names)
	}

	got, err := TaskRepoLinks(ctx, asks)
	if err != nil {
		t.Fatal(err)
	}
	if perTask != 0 || batched != 1 {
		t.Errorf("checking %d tasks took %d reads of one task's project or repositories and %d of all of them, want one of all of them",
			len(asks), perTask, batched)
	}
	if len(got) != len(want) {
		t.Errorf("%d tasks' repositories are linked, want %d: %v", len(got), len(want), got)
	}
	for ask := range want {
		if !got[ask] {
			t.Errorf("task %s's project links %s/%s, but the check says not", ask.Task, ask.Owner, ask.Name)
		}
	}
}
