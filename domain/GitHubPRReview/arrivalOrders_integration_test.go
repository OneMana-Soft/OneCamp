//go:build integration

package domain

// A reviewer's reviews read the same whatever order their deliveries arrive in
// (V-986, CR-B95 and CR-332): each written one by one, through the webhook's
// write (RecordGitHubPRReview), on a pull request of its own, leaves what
// foldReviews makes of them in the order they were submitted, which is
// GitHub's reading. Every sequence of up to three reviews of the four written
// states at distinct times, and every decision with its dismissal at the
// decision's time (alone, or with one review before or after), in every
// arrival order: 520 orders, every one read through the task's read (V-1060).
//
// Run: PKGS=./domain/GitHubPRReview/ integ_rel.sh <tree> <log> '^TestEveryArrivalOrderReadsAsTheReviewsInTimeOrder$'

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestEveryArrivalOrderReadsAsTheReviewsInTimeOrder(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	user, project, task := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id) VALUES ($1, $2)`, []any{user, user.String()[:8] + "@example.test"}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, []any{project, user}},
		{`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, []any{task, project, user}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}

	base := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	// A review's id is its minute: a dismissal carries the id and the time of
	// the review it dismisses.
	review := func(state string, minute int) Review {
		return Review{ID: int64(minute), Login: "ann", State: state, SubmittedAt: base.Add(time.Duration(minute) * time.Minute)}
	}
	// The sequences, each in the order its reviews were submitted (a
	// dismissal after the review it dismisses, at that review's time).
	written := []string{Approved, ChangesRequested, Commented, Dismissed}
	var sequences [][]Review
	var grow func(seq []Review)
	grow = func(seq []Review) {
		if len(seq) > 0 {
			sequences = append(sequences, slices.Clone(seq))
		}
		if len(seq) == 3 {
			return
		}
		for _, s := range written {
			grow(append(seq, review(s, len(seq)+1)))
		}
	}
	grow(nil)
	for _, decided := range []string{Approved, ChangesRequested} {
		pair := []Review{review(decided, 2), review(Dismissed, 2)}
		sequences = append(sequences, pair)
		for _, s := range written {
			sequences = append(sequences, append([]Review{review(s, 1)}, pair...), append(slices.Clone(pair), review(s, 3)))
		}
	}
	describe := func(reviews []Review) string {
		var s []string
		for _, r := range reviews {
			s = append(s, fmt.Sprintf("%s at %d", r.State, int(r.SubmittedAt.Sub(base)/time.Minute)))
		}
		return strings.Join(s, ", ")
	}

	// gaps counts the orders that read otherwise than GitHub does: none, now
	// that each review is a row of its own, folded on read (V-1060).
	cases, gaps := 0, 0
	for _, seq := range sequences {
		want := foldReviews(seq)[0].State
		if want == Dismissed {
			want = "" // a dismissed review stands for nothing
		}
		for _, order := range arrivalOrders(len(seq)) {
			cases++
			address := fmt.Sprintf("https://github.com/acme/orders/pull/%d", cases)
			if _, err := env.PG.Exec(`UPDATE tasks SET github_pr_url = $2 WHERE id = $1`, task, address); err != nil {
				t.Fatal(err)
			}
			var arrived []Review
			for _, i := range order {
				r := seq[i]
				arrived = append(arrived, r)
				// GitHub's webhook writes a state in lower case.
				r.State = strings.ToLower(r.State)
				if err := RecordGitHubPRReview(ctx, task, address, r); err != nil {
					t.Fatalf("%s: %v", describe(arrived), err)
				}
			}
			// What the task reads: its pull request's reviews, folded.
			rows, err := GetGitHubPRReviewsByTaskId(ctx, task)
			if err != nil {
				t.Fatalf("%s: %v", describe(arrived), err)
			}
			got := ""
			for _, r := range Standing(rows) {
				got = r.ReviewState
			}
			if got != want {
				gaps++
				t.Errorf("submitted %s, delivered %s: the task reads %q, GitHub reads %q", describe(seq), describe(arrived), got, want)
			}
		}
	}
	if cases != 520 || gaps != 0 {
		t.Errorf("%d arrival orders, %d of them read otherwise than GitHub: want 520 and 0", cases, gaps)
	}
}

// arrivalOrders is every order of n deliveries, as indexes into them.
func arrivalOrders(n int) [][]int {
	if n == 0 {
		return [][]int{nil}
	}
	var all [][]int
	for _, rest := range arrivalOrders(n - 1) {
		for i := 0; i <= len(rest); i++ {
			all = append(all, append(append(slices.Clone(rest[:i]), n-1), rest[i:]...))
		}
	}
	return all
}
