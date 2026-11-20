//go:build integration

package integration_test

// Reviews stored before 602 (V-1060), in 597's shape: one row per task, pull
// request and reviewer, with no review id. Migration 602 goes up, down and up
// again from such rows beside reviews by id, and the task reads the same. A
// decision from before 602 that GitHub has since dismissed (the dismissal names
// the review and carries its time, the old row's) folds after the old row, in
// every arrival order, one by one or as a batch.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^Test(Migration602RunsFromRowsWrittenBeforeIt|ADismissalOfADecisionFromBefore602FoldsAfterIt)$'

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

var oldRowsBase = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func oldRowsAt(m int) time.Time { return oldRowsBase.Add(time.Duration(m) * time.Minute) }

func oldRowsStanding(t *testing.T, task uuid.UUID) string {
	t.Helper()
	rs, err := githubPRReviewDomain.GetGitHubPRReviewsByTaskId(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, r := range githubPRReviewDomain.Standing(rs) {
		s = append(s, r.GitHubLogin+":"+r.ReviewState)
	}
	sort.Strings(s)
	return strings.Join(s, " ") + " => " + githubPRReviewDomain.Decision(rs)
}

func TestMigration602RunsFromRowsWrittenBeforeIt(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	ctx := context.Background()
	sql := func(name string) string {
		b, err := os.ReadFile("../../migrations/602_github_pr_review_ids." + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := sql("up"), sql("down")
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%.80s: %v", q, err)
		}
	}
	t.Cleanup(func() { _, _ = env.PG.Exec(up) })
	one := func(q string, args ...any) (n int) {
		t.Helper()
		if err := env.PG.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%.80s: %v", q, err)
		}
		return n
	}

	// To 601's shape (597's table), then seed old rows: one per task, PR, reviewer.
	run(down)
	if n := one(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'github_pr_reviews' AND column_name = 'review_id'`); n != 0 {
		t.Fatalf("after the first down review_id is still there")
	}
	user, project, task, task2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	const pr1, pr2, other = "https://github.com/acme/web/pull/1", "https://github.com/acme/web/pull/2", "https://github.com/acme/api/pull/9"
	run(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, user, user.String()[:8]+"@example.test")
	run(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, project, user)
	run(`INSERT INTO tasks (id, project_id, created_by, github_pr_url, github_pr_number) VALUES ($1, $2, $3, $4, 2)`, task, project, user, pr2)
	run(`INSERT INTO tasks (id, project_id, created_by, github_pr_url, github_pr_number) VALUES ($1, $2, $3, $4, 9)`, task2, project, user, other)
	legacyIDs := map[string]string{}
	for _, r := range []struct {
		task      uuid.UUID
		pr, login string
		state     string
		minute    int
	}{
		{task, pr2, "ann", "APPROVED", 1}, {task, pr2, "bo", "CHANGES_REQUESTED", 2},
		{task, pr2, "cy", "COMMENTED", 3}, {task, pr2, "dee", "DISMISSED", 4},
		{task, pr1, "eve", "APPROVED", 5}, {task2, other, "ann", "APPROVED", 1},
	} {
		var id string
		if err := env.PG.QueryRow(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state, submitted_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			r.task, r.pr, r.login, r.state, oldRowsAt(r.minute)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		legacyIDs[r.pr+" "+r.login] = id
	}

	run(up)
	if n := one(`SELECT count(*) FROM github_pr_reviews WHERE review_id IS NULL`); n != 6 {
		t.Errorf("after up, %d id-less rows, want the 6 old ones", n)
	}
	if n := one(`SELECT count(*) FROM pg_constraint WHERE conname = 'github_pr_reviews_task_pr_login_key'`); n != 0 {
		t.Errorf("after up, 597's constraint is still a constraint")
	}
	if n := one(`SELECT count(*) FROM pg_indexes WHERE tablename = 'github_pr_reviews' AND (
		(indexname = 'github_pr_reviews_task_pr_login_key' AND indexdef LIKE '%UNIQUE%(task_id, pr_url, github_login) WHERE (review_id IS NULL)')
		OR (indexname = 'github_pr_reviews_task_pr_review_key' AND indexdef LIKE '%UNIQUE%(task_id, pr_url, review_id) WHERE (review_id IS NOT NULL)'))`); n != 2 {
		t.Errorf("after up, %d of the two partial unique indexes", n)
	}
	if _, err := env.PG.Exec(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state, submitted_at) VALUES ($1, $2, 'ann', 'COMMENTED', now())`, task, pr2); err == nil {
		t.Errorf("after up, a second id-less row for ann on #2 was let in")
	}
	if got, want := oldRowsStanding(t, task), "ann:APPROVED bo:CHANGES_REQUESTED cy:COMMENTED => changes_requested"; got != want {
		t.Errorf("old rows after up read %q, want %q", got, want)
	}

	// Reviews by id since 602, beside the old rows; review 11 also on task2 (an id
	// collision across tasks must not touch task's row).
	write := func(tk uuid.UUID, pr string, rs ...githubPRReviewDomain.Review) {
		t.Helper()
		if err := githubPRReviewDomain.RecordGitHubPRReviews(ctx, tk, pr, rs); err != nil {
			t.Fatal(err)
		}
	}
	R := func(id int64, login, state string, m int) githubPRReviewDomain.Review {
		return githubPRReviewDomain.Review{ID: id, Login: login, State: state, SubmittedAt: oldRowsAt(m)}
	}
	write(task, pr2, R(11, "ann", "commented", 6), R(21, "bo", "approved", 7), R(31, "cy", "changes_requested", 8), R(32, "cy", "commented", 9),
		R(41, "dee", "commented", 10), R(51, "fay", "approved", 11), R(61, "gus", "commented", 12), R(62, "gus", "commented", 12))
	write(task, pr2, R(51, "fay", "dismissed", 11))
	write(task, pr1, R(71, "eve", "changes_requested", 6))
	write(task2, other, R(11, "ann", "approved", 2))
	before := oldRowsStanding(t, task)
	if want := "ann:APPROVED bo:APPROVED cy:CHANGES_REQUESTED dee:COMMENTED gus:COMMENTED => changes_requested"; before != want {
		t.Errorf("before down: %q, want %q", before, want)
	}
	if n := one(`SELECT count(*) FROM github_pr_reviews WHERE task_id = $1 AND review_id = 11 AND review_state = 'COMMENTED'`, task); n != 1 {
		t.Errorf("task2's review 11 changed task's review 11")
	}
	var gus62 string
	if err := env.PG.QueryRow(`SELECT id::text FROM github_pr_reviews WHERE task_id = $1 AND review_id = 62`, task).Scan(&gus62); err != nil {
		t.Fatal(err)
	}
	total := one(`SELECT count(*) FROM github_pr_reviews`)

	run(down)
	type kept struct {
		state  string
		minute int
	}
	rows := func() (map[string]kept, map[string]string) {
		t.Helper()
		got, ids := map[string]kept{}, map[string]string{}
		rs, err := env.PG.Query(`SELECT id, task_id, pr_url, github_login, review_state, submitted_at FROM github_pr_reviews`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var id string
			var tk uuid.UUID
			var pr, login, state string
			var at time.Time
			if err := rs.Scan(&id, &tk, &pr, &login, &state, &at); err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s %s %s", map[bool]string{true: "t1", false: "t2"}[tk == task], pr[len(pr)-1:], login)
			if _, twice := got[key]; twice {
				t.Errorf("%s kept twice", key)
			}
			got[key], ids[key] = kept{state, int(at.Sub(oldRowsBase) / time.Minute)}, id
		}
		return got, ids
	}
	want := map[string]kept{
		"t1 2 ann": {"APPROVED", 1}, "t1 2 bo": {"APPROVED", 7}, "t1 2 cy": {"CHANGES_REQUESTED", 8},
		"t1 2 dee": {"COMMENTED", 10}, "t1 2 fay": {"DISMISSED", 11}, "t1 2 gus": {"COMMENTED", 12},
		"t1 1 eve": {"CHANGES_REQUESTED", 6}, "t2 9 ann": {"APPROVED", 2},
	}
	got, ids := rows()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after down (from %d rows):\n got %v\nwant %v", total, got, want)
	}
	if ids["t1 2 ann"] != legacyIDs[pr2+" ann"] || ids["t1 2 gus"] != gus62 {
		t.Errorf("after down kept ann's row %s (want old %s) and gus's %s (want review 62's %s)", ids["t1 2 ann"], legacyIDs[pr2+" ann"], ids["t1 2 gus"], gus62)
	}
	if n := one(`SELECT count(*) FROM pg_constraint WHERE conname = 'github_pr_reviews_task_pr_login_key' AND contype = 'u'`); n != 1 {
		t.Errorf("after down, 597's constraint isn't back")
	}

	run(up)
	run(up)
	if got2, _ := rows(); fmt.Sprint(got2) != fmt.Sprint(want) {
		t.Errorf("after up, up: %v, want %v", got2, want)
	}
	if after := oldRowsStanding(t, task); after != before {
		t.Errorf("after down, up, up the task reads %q, before %q", after, before)
	}
}

// ann approved before 602 (an id-less row at her approval's time). Since, GitHub
// dismissed that approval: the dismissal names review 10 and carries its time,
// the same minute as the old row. Whether it comes alone, with ann's later
// reply, or in a Refresh batch, in any order, it must fold after the old row.
func TestADismissalOfADecisionFromBefore602FoldsAfterIt(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	ctx := context.Background()
	user, project := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id) VALUES ($1, $2)`, []any{user, user.String()[:8] + "@example.test"}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, []any{project, user}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	R := func(id int64, login, state string, m int) githubPRReviewDomain.Review {
		return githubPRReviewDomain.Review{ID: id, Login: login, State: state, SubmittedAt: oldRowsAt(m)}
	}
	perms := func(n int) [][]int {
		out := [][]int{{}}
		for k := 0; k < n; k++ {
			var next [][]int
			for _, p := range out {
				for i := 0; i <= len(p); i++ {
					q := append(append(append([]int{}, p[:i]...), k), p[i:]...)
					next = append(next, q)
				}
			}
			out = next
		}
		return out
	}
	cases := 0
	for ci, c := range []struct {
		reviews []githubPRReviewDomain.Review
		want    string
	}{
		{[]githubPRReviewDomain.Review{R(10, "ann", "dismissed", 1)}, " => "},
		{[]githubPRReviewDomain.Review{R(10, "ann", "dismissed", 1), R(11, "ann", "commented", 6)}, "ann:COMMENTED => commented"},
		{[]githubPRReviewDomain.Review{R(10, "ann", "DISMISSED", 1), R(11, "ann", "COMMENTED", 6), R(20, "bo", "APPROVED", 2)}, "ann:COMMENTED bo:APPROVED => approved"},
		{[]githubPRReviewDomain.Review{R(10, "ann", "dismissed", 1), R(12, "ann", "changes_requested", 1), R(13, "ann", "commented", 1)}, "ann:CHANGES_REQUESTED => changes_requested"},
	} {
		for _, order := range perms(len(c.reviews)) {
			for _, batch := range []bool{false, true} {
				cases++
				task := uuid.New()
				address := fmt.Sprintf("https://github.com/acme/web/pull/%d", 100+cases)
				if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by, github_pr_url) VALUES ($1, $2, $3, $4)`, task, project, user, address); err != nil {
					t.Fatal(err)
				}
				if _, err := env.PG.Exec(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state, submitted_at) VALUES ($1, $2, 'ann', 'APPROVED', $3)`, task, address, oldRowsAt(1)); err != nil {
					t.Fatal(err)
				}
				var arrived []githubPRReviewDomain.Review
				for _, i := range order {
					arrived = append(arrived, c.reviews[i])
				}
				if batch {
					if err := githubPRReviewDomain.RecordGitHubPRReviews(ctx, task, address, arrived); err != nil {
						t.Fatal(err)
					}
				} else {
					for _, r := range arrived {
						if err := githubPRReviewDomain.RecordGitHubPRReview(ctx, task, address, r); err != nil {
							t.Fatal(err)
						}
					}
				}
				if got := oldRowsStanding(t, task); got != c.want {
					t.Errorf("case %d, delivered %v (batch %v): reads %q, want %q", ci, order, batch, got, c.want)
				}
			}
		}
	}
	if cases != 2*(1+2+6+6) {
		t.Errorf("%d cases, want 30", cases)
	}
}
