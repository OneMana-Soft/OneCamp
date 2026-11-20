//go:build integration
// +build integration

package integration_test

// A task's pull request review reads as GitHub's review decision (V-986). A
// reply on a review thread reaches the webhook as a COMMENTED review, and
// every review a reviewer submitted replaced the last, so one who approved and
// then replied read as "Reviewed", and one who asked for changes and then
// commented stopped blocking. A dismissal never arrived (only "submitted" was
// read), and reviews were kept per task, so a task's next pull request carried
// the first one's reviews. Every delivery comes through the signed webhook,
// for a repository of this test's own (acme/prreview): the links are cached
// for five minutes, and other tests deliver for acme/web.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestAReviewersDecisionReadsAsGitHubReadsIt$'

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAReviewersDecisionReadsAsGitHubReadsIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	recordGitHub(t)

	// Website (key PRR) links acme/prreview, which syncs pull requests; its
	// tasks are PRR-1, PRR-2 and PRR-3.
	w := seedGitHubWorkspace(t, env, dg)
	w.exec(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, webhook_secret, created_by)
		VALUES ($1, 'acme', 'prreview', true, true, 'prreview-secret', $2)`, w.project, w.people["ana"])
	w.exec(`INSERT INTO project_keys (project_id, key, next_number) VALUES ($1, 'PRR', 4)`, w.project)
	for i, task := range []string{w.task, w.other, w.third} {
		w.exec(`UPDATE tasks SET task_number = $2 WHERE id = $1`, task, i+1)
	}

	at, sent := time.Now(), 0
	send := func(t *testing.T, event string, payload map[string]any) {
		t.Helper()
		sent++
		delivery := fmt.Sprintf("prreview-%d", sent)
		if code := w.deliver("acme/prreview", event, delivery, "prreview-secret", payload); code != http.StatusOK {
			t.Fatalf("the %s delivery: %d, want 200", event, code)
		}
		w.handled(t, delivery)
	}
	address := func(number int) string { return fmt.Sprintf("https://github.com/acme/prreview/pull/%d", number) }
	// open opens pull request number from branch for task PRR-key.
	open := func(t *testing.T, number, key int, branch string) {
		t.Helper()
		at = at.Add(time.Minute)
		send(t, "pull_request", map[string]any{"action": "opened", "pull_request": map[string]any{
			"number": number, "title": fmt.Sprintf("PRR-%d Tidy the form", key), "html_url": address(number), "state": "open",
			"user": map[string]any{"login": "author"}, "head": map[string]any{"ref": branch},
			"updated_at": at.UTC().Format(time.RFC3339)}})
	}
	// review delivers login's review of state on pull request number;
	// action is "submitted", or "dismissed" (GitHub then says the review's
	// state is "dismissed"). GitHub sends every review with its id, and a
	// dismissal with the id and time of the review it dismisses: here, a
	// review is its reviewer, pull request and time.
	ids := map[string]int64{}
	reviewAt := func(t *testing.T, action, state, login string, number int, branch string, submitted time.Time) {
		t.Helper()
		key := fmt.Sprint(login, number, submitted.Unix())
		if ids[key] == 0 {
			ids[key] = int64(len(ids) + 1)
		}
		send(t, "pull_request_review", map[string]any{"action": action, "review": map[string]any{
			"id": ids[key], "state": state, "body": "", "submitted_at": submitted.UTC().Format(time.RFC3339), "user": map[string]any{"login": login}},
			"pull_request": map[string]any{"number": number, "html_url": address(number), "head": map[string]any{"ref": branch}}})
	}
	review := func(t *testing.T, action, state, login string, number int, branch string) {
		t.Helper()
		at = at.Add(time.Minute)
		reviewAt(t, action, state, login, number, branch, at)
	}
	decision := func(t *testing.T, task string) string {
		t.Helper()
		var got string
		if err := env.PG.QueryRow(`SELECT COALESCE(github_pr_review_state, '') FROM tasks WHERE id = $1`, task).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("a reviewer who approves and then replies on a thread still approves", func(t *testing.T) {
		open(t, 11, 1, "prr-1-form")
		review(t, "submitted", "approved", "ann", 11, "prr-1-form")
		review(t, "submitted", "commented", "ann", 11, "prr-1-form")
		if got := decision(t, w.task); got != "approved" {
			t.Errorf("approved, then a reply: the task reads %q, want approved", got)
		}
	})

	t.Run("a reviewer who asked for changes and then comments still blocks another's approval", func(t *testing.T) {
		open(t, 12, 2, "prr-2-form")
		review(t, "submitted", "changes_requested", "bo", 12, "prr-2-form")
		review(t, "submitted", "approved", "cat", 12, "prr-2-form")
		review(t, "submitted", "commented", "bo", 12, "prr-2-form")
		if got := decision(t, w.other); got != "changes_requested" {
			t.Errorf("changes asked, approved by another, then a comment: the task reads %q, want changes_requested", got)
		}
	})

	t.Run("a dismissed request for changes leaves no decision, and the review is kept as dismissed", func(t *testing.T) {
		open(t, 13, 3, "prr-3-form")
		review(t, "submitted", "changes_requested", "dee", 13, "prr-3-form")
		reviewAt(t, "dismissed", "dismissed", "dee", 13, "prr-3-form", at)
		if got := decision(t, w.third); got != "" {
			t.Errorf("changes asked, then dismissed: the task reads %q, want no decision", got)
		}
		var state string
		if err := env.PG.QueryRow(`SELECT review_state FROM github_pr_reviews WHERE task_id = $1 AND github_login = 'dee'`, w.third).Scan(&state); err != nil || state != "DISMISSED" {
			t.Errorf("dee's review after the dismissal: %q (%v), want it kept as DISMISSED", state, err)
		}
	})

	t.Run("a task's next pull request starts with no review, and the first one's reviews stay with it", func(t *testing.T) {
		open(t, 21, 1, "prr-1-login")
		review(t, "submitted", "approved", "ann", 21, "prr-1-login")
		if got := decision(t, w.task); got != "approved" {
			t.Fatalf("#21 approved: the task reads %q, want approved", got)
		}
		// #21 closes unmerged and #22 opens from the same branch.
		open(t, 22, 1, "prr-1-login")
		if got := decision(t, w.task); got != "" {
			t.Errorf("#22 opened: the task reads %q, want no decision yet", got)
		}
		review(t, "submitted", "commented", "eve", 22, "prr-1-login")
		if got := decision(t, w.task); got != "commented" {
			t.Errorf("eve comments on #22: the task reads %q, want commented", got)
		}
		// A late review on #21 is #21's.
		review(t, "submitted", "changes_requested", "ann", 21, "prr-1-login")
		if got := decision(t, w.task); got != "commented" {
			t.Errorf("ann asks for changes on #21: the task (on #22) reads %q, want commented", got)
		}
	})
	// GitHub doesn't promise the order it delivers in, and a delivery that
	// failed is delivered again later: the review submitted last decides,
	// whichever arrives last. A dismissal carries the time of the review it
	// dismisses.
	t.Run("an approval delivered after a later request for changes, or its dismissal, leaves changes_requested", func(t *testing.T) {
		open(t, 23, 2, "prr-2-late")
		at = at.Add(time.Minute)
		approved := at
		review(t, "submitted", "changes_requested", "ann", 23, "prr-2-late")
		reviewAt(t, "submitted", "approved", "ann", 23, "prr-2-late", approved)
		if got := decision(t, w.other); got != "changes_requested" {
			t.Errorf("changes asked after an approval that arrived late: the task reads %q, want changes_requested", got)
		}
		reviewAt(t, "dismissed", "dismissed", "ann", 23, "prr-2-late", approved)
		if got := decision(t, w.other); got != "changes_requested" {
			t.Errorf("then that approval dismissed: the task reads %q, want changes_requested", got)
		}
	})
	// A later reply keeps an earlier decision, so a reply stored first doesn't
	// outrank the approval it followed (CR-332's G1); a dismissal comes after
	// the review it dismisses, at its time, so that review's own submit
	// delivered late doesn't undo it (G2).
	t.Run("a reply delivered before the approval it followed leaves approved", func(t *testing.T) {
		open(t, 31, 3, "prr-3-reply-first")
		approvedAt := at.Add(time.Minute)
		at = at.Add(5 * time.Minute)
		reviewAt(t, "submitted", "commented", "gus", 31, "prr-3-reply-first", at)
		reviewAt(t, "submitted", "approved", "gus", 31, "prr-3-reply-first", approvedAt)
		if got := decision(t, w.third); got != "approved" {
			t.Errorf("approved at t1, replied at t5, the reply delivered first: the task reads %q, want approved", got)
		}
	})
	t.Run("a submit delivered after its own dismissal leaves no decision", func(t *testing.T) {
		open(t, 32, 3, "prr-3-dismissed-first")
		at = at.Add(time.Minute)
		reviewAt(t, "dismissed", "dismissed", "hal", 32, "prr-3-dismissed-first", at)
		reviewAt(t, "submitted", "approved", "hal", 32, "prr-3-dismissed-first", at)
		if got := decision(t, w.third); got != "" {
			t.Errorf("an approval at t1 dismissed, its submit delivered after the dismissal: the task reads %q, want no decision", got)
		}
	})
}

// Migration 597 goes down and up again with reviews on two of a task's pull
// requests, and the task reads the same review decision after it as before:
// down drops exactly the reviews the task's read leaves out (another pull
// request's, or dismissed), so what stays holds the old key and nothing hidden
// comes back as a visible review; up gives every review the task's pull
// request and takes DISMISSED and a second pull request's review again (and
// runs twice). A task whose pull request address is no GitHub address (a
// near-limit run of text) doesn't stop up, and its review stays unread (one
// from before 597 goes on down); a
// task that holds no pull request reads none of its old one's reviews.
func TestMigration597ReviewsPerPullRequestGoDownAndUpAgain(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("../../migrations/597_github_pr_review_per_pr." + name + ".sql")
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
	// However the test ends, the tests after it get the schema up leaves.
	up := read("up")
	t.Cleanup(func() {
		if _, err := env.PG.Exec(up); err != nil {
			t.Errorf("597 up again after the test: %v", err)
		}
	})
	user, project, task := uuid.New(), uuid.New(), uuid.New()
	const first, now = "https://github.com/acme/web/pull/1", "https://github.com/acme/web/pull/2"
	run(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, user, user.String()[:8]+"@example.test")
	run(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, project, user)
	run(`INSERT INTO tasks (id, project_id, created_by, github_pr_url, github_pr_number) VALUES ($1, $2, $3, $4, 2)`, task, project, user, now)
	// ann approved #1 and was dismissed on #2; bo asked for changes on #1 only;
	// cy approved #2 and, later, commented on #1.
	for _, r := range []struct {
		pr, login, state string
		ago              int
	}{
		{first, "ann", "APPROVED", 9}, {now, "ann", "DISMISSED", 8},
		{first, "bo", "CHANGES_REQUESTED", 7},
		{now, "cy", "APPROVED", 6}, {first, "cy", "COMMENTED", 5},
	} {
		run(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state, submitted_at)
			VALUES ($1, $2, $3, $4, NOW() - make_interval(mins => $5))`, task, r.pr, r.login, r.state, r.ago)
	}
	// A task that holds no pull request now (it was unlinked, or GitHub was
	// disconnected) keeps eve's request for changes on its old one, #1.
	unlinked := uuid.New()
	run(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, unlinked, project, user)
	run(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state) VALUES ($1, $2, 'eve', 'CHANGES_REQUESTED')`, unlinked, first)
	reviews := func() map[string]string {
		t.Helper()
		got := map[string]string{}
		rows, err := env.PG.Query(`SELECT github_login, review_state FROM github_pr_reviews WHERE task_id = $1`, task)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var login, state string
			if err := rows.Scan(&login, &state); err != nil {
				t.Fatal(err)
			}
			if _, twice := got[login]; twice {
				t.Errorf("%s has two reviews", login)
			}
			got[login] = state
		}
		return got
	}

	// What the task reads: its reviewers' reviews and its decision.
	reads := func(task uuid.UUID) string {
		t.Helper()
		rs, err := githubPRReviewDomain.GetGitHubPRReviewsByTaskId(context.Background(), task)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, r := range githubPRReviewDomain.Standing(rs) {
			got[r.GitHubLogin] = r.ReviewState
		}
		return fmt.Sprintf("%v %q", got, githubPRReviewDomain.Decision(rs))
	}
	before := reads(task)
	if got := reads(unlinked); got != `map[] ""` {
		t.Fatalf("the task with no pull request reads %s before down, want no review", got)
	}
	// A task whose pull request address is a run of random text near the
	// index limit. Its review from before 597 is under no address (up gives
	// none over 512 characters), so the task reads none, and down drops it
	// with the other reviews no read takes.
	long, odd := make([]byte, 2048), uuid.New()
	if _, err := rand.Read(long); err != nil {
		t.Fatal(err)
	}
	address := "https://github.com/acme/web/pull/" + base64.RawURLEncoding.EncodeToString(long)
	address = address[:2680]
	run(`INSERT INTO tasks (id, project_id, created_by, github_pr_url, github_pr_number) VALUES ($1, $2, $3, $4, 3)`, odd, project, user, address)
	run(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state) VALUES ($1, '', 'dee', 'CHANGES_REQUESTED')`, odd)
	if got := reads(odd); got != `map[] ""` {
		t.Fatalf("the task with a %d-byte address reads %s before down, want no review", len(address), got)
	}

	run(read("down"))
	var left int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM github_pr_reviews WHERE task_id = $1`, odd).Scan(&left); err != nil || left != 0 {
		t.Errorf("after down, the task with a %d-byte address keeps %d reviews (%v), want none", len(address), left, err)
	}
	want := map[string]string{"cy": "APPROVED"}
	if got := reviews(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after down: %v, want %v (only what the task read)", got, want)
	}
	if _, err := env.PG.Exec(`INSERT INTO github_pr_reviews (task_id, github_login, review_state) VALUES ($1, 'cy', 'COMMENTED')`, task); err == nil {
		t.Error("after down, a second review of cy's on the task was taken; the old key holds one")
	}
	// Before up, that task reviewed again, under the old key.
	run(`INSERT INTO github_pr_reviews (task_id, github_login, review_state) VALUES ($1, 'dee', 'CHANGES_REQUESTED')`, odd)

	run(read("up"))
	run(read("up"))
	if got := reviews(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after up: %v, want %v", got, want)
	}
	if after := reads(task); after != before {
		t.Errorf("the task reads %s after down and up, %s before", after, before)
	}
	if got := reads(odd); got != `map[] ""` {
		t.Errorf("the task with a %d-byte address reads %s, want no review", len(address), got)
	}
	// Down drops eve's review, which the task's read leaves out. Kept, it
	// would come back from up under no address, which is what the read of a
	// task holding none compares, and show again.
	if got := reads(unlinked); got != `map[] ""` {
		t.Errorf("the task with no pull request reads %s after down and up, want no review", got)
	}
	var off int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM github_pr_reviews WHERE task_id = $1 AND pr_url <> $2`, task, now).Scan(&off); err != nil || off != 0 {
		t.Errorf("after up, %d reviews (%v) aren't on the task's pull request", off, err)
	}
	run(`INSERT INTO github_pr_reviews (task_id, pr_url, github_login, review_state) VALUES ($1, $2, 'bo', 'DISMISSED')`, task, first)
}
