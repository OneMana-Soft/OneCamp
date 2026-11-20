//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Migration 602 (V-1060) keeps each review as a row of its own, by GitHub's
// review id. Down goes back to 597's one review per task, pull request and
// reviewer, keeping each reviewer's folded review (what the task's read takes
// from all of theirs, ties by review id) and nothing else; up takes the ids
// again (a review from before has none), and runs twice. The task decides the
// same before and after.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestMigration602ReviewIdsGoDownAndUpAgain$'
func TestMigration602ReviewIdsGoDownAndUpAgain(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("../../migrations/602_github_pr_review_ids." + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	up := read("up")
	t.Cleanup(func() {
		if _, err := env.PG.Exec(up); err != nil {
			t.Errorf("602 up again after the test: %v", err)
		}
	})
	user, project, task := uuid.New(), uuid.New(), uuid.New()
	const first, now = "https://github.com/acme/web/pull/1", "https://github.com/acme/web/pull/2"
	run(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, user, user.String()[:8]+"@example.test")
	run(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, project, user)
	run(`INSERT INTO tasks (id, project_id, created_by, github_pr_url, github_pr_number) VALUES ($1, $2, $3, $4, 2)`, task, project, user, now)
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	// ann approved before 602 (no id) and replied since; bo asked for
	// changes, then approved (dismissed), then replied; cy approved, then
	// asked for changes; eve approved and replied at the same minute (the
	// approval's id first); gil approved before 602 (no id) and that approval
	// was dismissed after it, at the same minute (the row with no id first);
	// dee, on the task's first pull request, approved and replied.
	for _, r := range []struct {
		pr, login, state string
		id               int64 // 0: none, a review from before 602
		minute           int
	}{
		{now, "ann", "APPROVED", 0, 1}, {now, "ann", "COMMENTED", 11, 5},
		{now, "bo", "CHANGES_REQUESTED", 21, 2}, {now, "bo", "DISMISSED", 22, 4}, {now, "bo", "COMMENTED", 23, 6},
		{now, "cy", "APPROVED", 31, 3}, {now, "cy", "CHANGES_REQUESTED", 32, 7},
		{now, "eve", "COMMENTED", 51, 8}, {now, "eve", "APPROVED", 50, 8},
		{now, "gil", "DISMISSED", 61, 9}, {now, "gil", "APPROVED", 0, 9},
		{first, "dee", "APPROVED", 41, 8}, {first, "dee", "COMMENTED", 42, 9},
	} {
		run(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state, review_id, submitted_at)
			VALUES ($1, $2, $3, $4, NULLIF($5::bigint, 0), $6)`, task, r.pr, r.login, r.state, r.id, base.Add(time.Duration(r.minute)*time.Minute))
	}
	decides := func() string {
		t.Helper()
		rs, err := githubPRReviewDomain.GetGitHubPRReviewsByTaskId(context.Background(), task)
		if err != nil {
			t.Fatal(err)
		}
		return githubPRReviewDomain.Decision(rs)
	}
	// rows is each review, as pull request, reviewer and state, at its minute;
	// a reviewer with two reviews on a pull request is an error.
	rows := func() map[string]int {
		t.Helper()
		got, seen := map[string]int{}, map[string]bool{}
		rs, err := env.PG.Query(`SELECT pr_url, github_login, review_state, submitted_at FROM github_pr_reviews WHERE task_id = $1`, task)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var pr, login, state string
			var at time.Time
			if err := rs.Scan(&pr, &login, &state, &at); err != nil {
				t.Fatal(err)
			}
			key := pr[len(pr)-1:] + " " + login
			if _, twice := seen[key]; twice {
				t.Errorf("%s has two reviews on #%s", login, pr[len(pr)-1:])
			}
			seen[key] = true
			got[key+" "+state] = int(at.Sub(base) / time.Minute)
		}
		return got
	}
	if got := decides(); got != "changes_requested" {
		t.Fatalf("before down the task decides %q, want changes_requested", got)
	}

	run(read("down"))
	// Each reviewer's folded review, alone.
	want := fmt.Sprint(map[string]int{
		"2 ann APPROVED": 1, "2 bo COMMENTED": 6, "2 cy CHANGES_REQUESTED": 7,
		"2 eve APPROVED": 8, "2 gil DISMISSED": 9, "1 dee APPROVED": 8,
	})
	if got := fmt.Sprint(rows()); got != want {
		t.Errorf("after down: %v, want %v", got, want)
	}
	var key string
	if err := env.PG.QueryRow(`SELECT contype FROM pg_constraint WHERE conname = 'github_pr_reviews_task_pr_login_key'`).Scan(&key); err != nil || key != "u" {
		t.Errorf("after down, 597's key is %q (%v), want a unique constraint", key, err)
	}

	run(up)
	run(up)
	if got := fmt.Sprint(rows()); got != want {
		t.Errorf("after up: %v, want %v", got, want)
	}
	if got := decides(); got != "changes_requested" {
		t.Errorf("after down and up the task decides %q, want changes_requested", got)
	}
	var ids, partial int
	if err := env.PG.QueryRow(`SELECT count(review_id) FROM github_pr_reviews WHERE task_id = $1`, task).Scan(&ids); err != nil || ids != 0 {
		t.Errorf("after up, %d reviews (%v) carry an id, want none: down's rows are folded decisions", ids, err)
	}
	if err := env.PG.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename = 'github_pr_reviews'
		AND indexname IN ('github_pr_reviews_task_pr_review_key', 'github_pr_reviews_task_pr_login_key')
		AND indexdef LIKE 'CREATE UNIQUE INDEX%(task_id, pr_url, %) WHERE %'`).Scan(&partial); err != nil || partial != 2 {
		t.Errorf("after up, %d partial unique keys (%v), want 2", partial, err)
	}
	// A review with an id and one without each find their key.
	run(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state, review_id) VALUES ($1, $2, 'cy', 'COMMENTED', 33)
		ON CONFLICT (task_id, pr_url, review_id) WHERE review_id IS NOT NULL DO NOTHING`, task, now)
	run(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state) VALUES ($1, $2, 'cy', 'COMMENTED')
		ON CONFLICT (task_id, pr_url, github_login) WHERE review_id IS NULL DO NOTHING`, task, now)
}
