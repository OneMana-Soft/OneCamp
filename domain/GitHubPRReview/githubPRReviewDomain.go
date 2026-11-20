package domain

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
	prReviewModel "github.com/akashc777/OneCamp/models/postgres/GitHubPRReview"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// A reviewer's review of a pull request, as GitHub names its state (stored
// upper-cased). A reply on a review thread reaches OneCamp as a COMMENTED
// review; a dismissed review is DISMISSED.
const (
	Approved         = "APPROVED"
	ChangesRequested = "CHANGES_REQUESTED"
	Commented        = "COMMENTED"
	Dismissed        = "DISMISSED"
)

// maxReviewedAddress is the longest pull request address, in characters, a
// review is kept under: the one 597 gives reviews when it backfills, so the
// key (task, address, reviewer) stays well inside Postgres's index limit
// (2,704 bytes; 512 characters are 2,048 bytes at most).
const maxReviewedAddress = 512

// kept is GitHub's review decision as one rule, the only copy of it: the
// reviews a reviewer's next review, of state next, leaves standing. A COMMENTED
// review never replaces the reviewer's APPROVED or CHANGES_REQUESTED; an
// APPROVED, CHANGES_REQUESTED or DISMISSED one replaces whatever stood (a
// dismissal clears the decision, and is kept as DISMISSED: every delete is
// soft). foldReviews applies it, on read (Standing).
func kept(next string) []string {
	if next == Commented {
		return []string{Approved, ChangesRequested}
	}
	return nil
}

// written is the states a review is written in.
var written = []string{Approved, ChangesRequested, Commented, Dismissed}

// recorded reports whether a review of state is one a pull request's review
// decision reads: GitHub's list of a pull request's reviews also holds the
// token holder's unsent one (PENDING), which isn't a review yet.
func recorded(state string) bool {
	return slices.Contains(written, state)
}

// A Review is one of a reviewer's reviews of a pull request, as GitHub sends
// it: ID is GitHub's review id (0 when there is none, as for the demo's
// landed change), State as GitHub writes it, any case. A dismissal is the
// review it dismisses, of state DISMISSED: its id and its time.
type Review struct {
	ID          int64
	Login       string
	AvatarURL   string
	HTMLURL     string
	State       string
	SubmittedAt time.Time
}

// RecordGitHubPRReview records review on the pull request at prURL, for the
// task: RecordGitHubPRReviews with one review.
func RecordGitHubPRReview(ctx context.Context, taskId uuid.UUID, prURL string, review Review) error {
	return RecordGitHubPRReviews(ctx, taskId, prURL, []Review{review})
}

// RecordGitHubPRReviews records reviews of the pull request at prURL, for the
// task, in one statement. It is the one write of a review, for the webhook,
// the sync and the demo's landed change alike. Each review with an id is a row
// of its own, per task and pull request, and the read folds a reviewer's rows
// (Standing), so they read the same whatever order they arrive in: a
// dismissal marks the review it names DISMISSED (inserting it, at the
// review's time, when it arrives first), and a submit delivered after its own
// dismissal leaves it DISMISSED. A review with no id keeps one row per
// reviewer (597's key), the newer replacing the older and a dismissal at its
// time clearing it. A state the decision doesn't read (PENDING) is not
// written. A pull request address over maxReviewedAddress characters keeps no
// review.
func RecordGitHubPRReviews(ctx context.Context, taskId uuid.UUID, prURL string, reviews []Review) error {
	rows := byReview(reviews)
	if len(rows) == 0 {
		return nil
	}
	if n := utf8.RuneCountInString(prURL); n > maxReviewedAddress {
		helpers.LogWarnWithContext(ctx, "domain/RecordGitHubPRReviews: task %s's pull request address is %d characters, over %d: its reviews aren't kept", taskId, n, maxReviewedAddress)
		return nil
	}
	var ids []int64
	var logins, avatars, htmls, states []string
	var times []time.Time
	for _, r := range rows {
		ids = append(ids, r.ID)
		logins = append(logins, r.Login)
		avatars = append(avatars, r.AvatarURL)
		htmls = append(htmls, r.HTMLURL)
		states = append(states, r.State)
		times = append(times, r.SubmittedAt)
	}
	// A review keeps the earliest time it arrived with: a delivery with no
	// time is stamped when it arrives (business reviewTime).
	query := `
		WITH r AS (
			SELECT NULLIF(r.review_id, 0) AS review_id, r.login, r.avatar_url, r.html_url, r.state, r.submitted_at
			FROM unnest($3::bigint[], $4::text[], $5::text[], $6::text[], $7::text[], $8::timestamptz[]) AS r(review_id, login, avatar_url, html_url, state, submitted_at)
		), by_id AS (
			INSERT INTO github_pr_reviews (task_id, pr_url, review_id, github_login, github_avatar_url, github_html_url, review_state, submitted_at)
			SELECT $1::uuid, $2::text, r.review_id, r.login, r.avatar_url, r.html_url, r.state, r.submitted_at FROM r WHERE r.review_id IS NOT NULL
			ON CONFLICT (task_id, pr_url, review_id) WHERE review_id IS NOT NULL DO UPDATE SET
				review_state = CASE WHEN github_pr_reviews.review_state = '` + Dismissed + `' THEN github_pr_reviews.review_state ELSE EXCLUDED.review_state END,
				submitted_at = LEAST(github_pr_reviews.submitted_at, EXCLUDED.submitted_at),
				github_avatar_url = EXCLUDED.github_avatar_url,
				github_html_url = EXCLUDED.github_html_url
		)
		INSERT INTO github_pr_reviews (task_id, pr_url, github_login, github_avatar_url, github_html_url, review_state, submitted_at)
		SELECT $1::uuid, $2::text, r.login, r.avatar_url, r.html_url, r.state, r.submitted_at FROM r WHERE r.review_id IS NULL
		ON CONFLICT (task_id, pr_url, github_login) WHERE review_id IS NULL DO UPDATE SET
			review_state = EXCLUDED.review_state,
			submitted_at = EXCLUDED.submitted_at,
			github_avatar_url = EXCLUDED.github_avatar_url,
			github_html_url = EXCLUDED.github_html_url
			WHERE (EXCLUDED.submitted_at, EXCLUDED.review_state = '` + Dismissed + `') >= (github_pr_reviews.submitted_at, github_pr_reviews.review_state = '` + Dismissed + `')
	`
	if _, err := prReviewModel.ExecRawContext(ctx, query, taskId, prURL, pq.Array(ids), pq.Array(logins), pq.Array(avatars), pq.Array(htmls), pq.Array(states), pq.Array(times)); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RecordGitHubPRReviews Failed err: %+v", err)
		return err
	}
	return nil
}

// byReview is reviews as the rows one statement writes, each at most once
// (Postgres refuses to change a row twice in one statement), states
// upper-cased and the ones the decision doesn't read dropped: one per review
// id (DISMISSED when any delivery of it is, else its last), then the reviews
// with no id folded to one per reviewer (foldReviews).
func byReview(reviews []Review) []Review {
	var rows, idless []Review
	at := map[int64]int{}
	for _, r := range reviews {
		r.State = strings.ToUpper(r.State)
		switch {
		case !recorded(r.State):
		case r.ID == 0:
			idless = append(idless, r)
		default:
			i, seen := at[r.ID]
			if !seen {
				at[r.ID] = len(rows)
				rows = append(rows, r)
			} else if rows[i].State != Dismissed {
				rows[i] = r
			}
		}
	}
	return append(rows, foldReviews(idless)...)
}

// foldReviews folds reviews, in the order they were submitted (ties in the
// order given), into the one that stands per reviewer (kept), in the order
// each first reviewed; states are upper-cased and the ones the decision
// doesn't read are dropped.
func foldReviews(reviews []Review) []Review {
	var rows []Review
	at := map[string]int{}
	reviews = slices.Clone(reviews)
	slices.SortStableFunc(reviews, func(a, b Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	for _, r := range reviews {
		r.State = strings.ToUpper(r.State)
		if !recorded(r.State) {
			continue
		}
		i, seen := at[r.Login]
		if !seen {
			at[r.Login] = len(rows)
			rows = append(rows, r)
			continue
		}
		f := &rows[i]
		if !slices.Contains(kept(r.State), f.State) {
			f.ID, f.State, f.SubmittedAt = r.ID, r.State, r.SubmittedAt
		}
		f.AvatarURL, f.HTMLURL = r.AvatarURL, r.HTMLURL
	}
	return rows
}

// GetGitHubPRReviewsByTaskId returns every review of the pull request the
// task has now (a task's earlier pull request's reviews are that pull
// request's), dismissed ones included, in the order they were submitted (ties
// by review id, a review with none first): what Standing folds.
func GetGitHubPRReviewsByTaskId(ctx context.Context, taskId uuid.UUID) ([]*prReviewModel.GitHubPRReview, error) {
	query := `
		SELECT ` + prReviewModel.GITHUB_PR_REVIEW_COLS + `
		FROM github_pr_reviews
		WHERE task_id = $1::uuid
			AND pr_url = COALESCE((SELECT github_pr_url FROM tasks WHERE id = $1::uuid), '')
		ORDER BY submitted_at, review_id NULLS FIRST
	`
	reviews, err := prReviewModel.GetGitHubPRReviewsByTaskId(ctx, query, taskId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubPRReviewsByTaskId Failed err: %+v", err)
		return nil, err
	}
	return reviews, nil
}

// Standing is the review that stands for each reviewer of a pull request,
// newest first, from its reviews in the order they were submitted
// (GetGitHubPRReviewsByTaskId's): foldReviews over each reviewer's, a review
// from before 602 (no id: a reviewer's folded decision) among them at its
// time. Each is the row of the review that stands. A reviewer whose standing
// review is dismissed has none: it counts for nothing.
func Standing(reviews []*prReviewModel.GitHubPRReview) []*prReviewModel.GitHubPRReview {
	rs := make([]Review, 0, len(reviews))
	for i, r := range reviews {
		// ID carries the row's index through the fold.
		rs = append(rs, Review{ID: int64(i), Login: r.GitHubLogin, State: r.ReviewState, SubmittedAt: r.SubmittedAt})
	}
	standing := make([]*prReviewModel.GitHubPRReview, 0, len(reviews))
	for _, f := range foldReviews(rs) {
		if f.State != Dismissed {
			standing = append(standing, reviews[f.ID])
		}
	}
	slices.SortStableFunc(standing, func(a, b *prReviewModel.GitHubPRReview) int { return b.SubmittedAt.Compare(a.SubmittedAt) })
	return standing
}

// Decision is a pull request's review decision from its reviews
// (GetGitHubPRReviewsByTaskId's), as a task stores it: "changes_requested"
// while any reviewer's standing review (Standing) asks for changes, else
// "approved" when any approves, else "commented" when anyone has reviewed,
// else "" (no decision).
func Decision(reviews []*prReviewModel.GitHubPRReview) string {
	decision := ""
	for _, r := range Standing(reviews) {
		switch r.ReviewState {
		case ChangesRequested:
			return "changes_requested"
		case Approved:
			decision = "approved"
		case Commented:
			if decision == "" {
				decision = "commented"
			}
		}
	}
	return decision
}
