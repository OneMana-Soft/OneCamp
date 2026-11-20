//go:build integration

package domain

// Each review is a row of its own, by GitHub's review id, folded on read
// (V-1060): a dismissal clears only the review it names, a submit delivered
// after its own dismissal stays dismissed, a review from before 602 (no id,
// a reviewer's folded decision) still folds in, and a review with no id (the
// demo's) keeps one row per reviewer.
//
// Run: PKGS=./domain/GitHubPRReview/ integ_rel.sh <tree> <log> '^TestA(DismissalClearsOnlyTheReviewItNames|SubmitDeliveredAfterItsOwnDismissalReadsNone|ReviewFromBefore602StillFoldsIn|ReviewWithNoIdKeepsOneRowPerReviewer)$'

import (
	"context"
	"fmt"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// reviewedTask is a task holding the pull request at address, on a database
// of its own, and what it reads: its reviewers' standing reviews and its
// decision.
func reviewedTask(t *testing.T, address string) (*integration.Env, uuid.UUID, func() string) {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	user, project, task := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id) VALUES ($1, $2)`, []any{user, user.String()[:8] + "@example.test"}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, []any{project, user}},
		{`INSERT INTO tasks (id, project_id, created_by, github_pr_url) VALUES ($1, $2, $3, $4)`, []any{task, project, user, address}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	reads := func() string {
		t.Helper()
		rows, err := GetGitHubPRReviewsByTaskId(context.Background(), task)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, r := range Standing(rows) {
			got[r.GitHubLogin] = r.ReviewState
		}
		return fmt.Sprintf("%v %q", got, Decision(rows))
	}
	return env, task, reads
}

// ann approves (review 1), then asks for changes (review 2); then her
// approval is dismissed. Her request for changes stands, in whatever order
// the three deliveries arrive, one by one or as one batch.
func TestADismissalClearsOnlyTheReviewItNames(t *testing.T) {
	const address = "https://github.com/acme/web/pull/1"
	env, task, reads := reviewedTask(t, address)
	ctx := context.Background()
	reviews := []Review{
		{ID: 1, Login: "ann", State: "approved", SubmittedAt: at(1)},
		{ID: 2, Login: "ann", State: "changes_requested", SubmittedAt: at(2)},
		{ID: 1, Login: "ann", State: "dismissed", SubmittedAt: at(1)},
	}
	for _, order := range arrivalOrders(len(reviews)) {
		for _, batch := range []bool{false, true} {
			if _, err := env.PG.Exec(`DELETE FROM github_pr_reviews WHERE task_id = $1`, task); err != nil {
				t.Fatal(err)
			}
			var arrived []Review
			for _, i := range order {
				arrived = append(arrived, reviews[i])
			}
			if batch {
				if err := RecordGitHubPRReviews(ctx, task, address, arrived); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, r := range arrived {
					if err := RecordGitHubPRReview(ctx, task, address, r); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got, want := reads(), `map[ann:CHANGES_REQUESTED] "changes_requested"`; got != want {
				t.Errorf("delivered %v (batch %v): the task reads %s, want %s", order, batch, got, want)
			}
			var rows, dismissed int
			if err := env.PG.QueryRow(`SELECT count(*), count(*) FILTER (WHERE review_id = 1 AND review_state = 'DISMISSED') FROM github_pr_reviews WHERE task_id = $1`, task).Scan(&rows, &dismissed); err != nil {
				t.Fatal(err)
			}
			if rows != 2 || dismissed != 1 {
				t.Errorf("delivered %v (batch %v): %d rows, %d of them review 1 dismissed; want 2 and 1", order, batch, rows, dismissed)
			}
		}
	}
}

// bo's approval is dismissed, and the dismissal arrives first; the approval's
// own delivery, late, leaves it dismissed: the task reads no review of his.
func TestASubmitDeliveredAfterItsOwnDismissalReadsNone(t *testing.T) {
	const address = "https://github.com/acme/web/pull/2"
	_, task, reads := reviewedTask(t, address)
	ctx := context.Background()
	for _, r := range []Review{
		{ID: 7, Login: "bo", State: "dismissed", SubmittedAt: at(3)},
		{ID: 7, Login: "bo", State: "approved", SubmittedAt: at(3)},
	} {
		if err := RecordGitHubPRReview(ctx, task, address, r); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := reads(), `map[] ""`; got != want {
		t.Errorf("the task reads %s, want %s", got, want)
	}
}

// Rows from before 602 carry no review id: each is a reviewer's folded
// decision, and folds in at its time. cy's approval from before stands
// through her later reply; dee's request for changes from before still
// decides.
func TestAReviewFromBefore602StillFoldsIn(t *testing.T) {
	const address = "https://github.com/acme/web/pull/3"
	env, task, reads := reviewedTask(t, address)
	for _, r := range []struct{ login, state string }{{"cy", "APPROVED"}, {"dee", "CHANGES_REQUESTED"}} {
		if _, err := env.PG.Exec(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state, submitted_at) VALUES ($1, $2, $3, $4, $5)`,
			task, address, r.login, r.state, at(1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordGitHubPRReview(context.Background(), task, address, Review{ID: 9, Login: "cy", State: "commented", SubmittedAt: at(2)}); err != nil {
		t.Fatal(err)
	}
	if got, want := reads(), `map[cy:APPROVED dee:CHANGES_REQUESTED] "changes_requested"`; got != want {
		t.Errorf("the task reads %s, want %s", got, want)
	}
}

// A review with no id (the demo's landed change writes one) keeps one row per
// reviewer, beside the reviews with ids: the newer replaces it, whichever
// arrives first, and a dismissal at its time clears it.
func TestAReviewWithNoIdKeepsOneRowPerReviewer(t *testing.T) {
	const address = "https://github.com/acme/web/pull/4"
	env, task, reads := reviewedTask(t, address)
	ctx := context.Background()
	write := func(rs ...Review) {
		t.Helper()
		for _, r := range rs {
			if err := RecordGitHubPRReview(ctx, task, address, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(Review{Login: "eve", State: "changes_requested", SubmittedAt: at(2)}, Review{Login: "eve", State: "approved", SubmittedAt: at(1)})
	write(Review{ID: 4, Login: "fay", State: "commented", SubmittedAt: at(1)}, Review{Login: "fay", State: "approved", SubmittedAt: at(2)})
	if got, want := reads(), `map[eve:CHANGES_REQUESTED fay:APPROVED] "changes_requested"`; got != want {
		t.Errorf("the task reads %s, want %s", got, want)
	}
	var eve int
	if err := env.PG.QueryRow(`SELECT count(*) FROM github_pr_reviews WHERE task_id = $1 AND github_login = 'eve'`, task).Scan(&eve); err != nil {
		t.Fatal(err)
	}
	if eve != 1 {
		t.Errorf("eve's reviews with no id keep %d rows, want 1", eve)
	}
	write(Review{Login: "eve", State: "dismissed", SubmittedAt: at(2)}, Review{Login: "eve", State: "changes_requested", SubmittedAt: at(2)})
	if got, want := reads(), `map[fay:APPROVED] "approved"`; got != want {
		t.Errorf("after eve's dismissal the task reads %s, want %s", got, want)
	}
}
