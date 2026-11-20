package domain

import (
	"slices"
	"testing"
	"time"

	prReviewModel "github.com/akashc777/OneCamp/models/postgres/GitHubPRReview"
)

// at is a review's time: minute minutes into the day the tests read.
func at(minute int) time.Time {
	return time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute)
}

// reviewsOf is login's reviews of these states, a minute apart, as the task's
// read returns them.
func reviewsOf(login string, states ...string) []*prReviewModel.GitHubPRReview {
	var rs []*prReviewModel.GitHubPRReview
	for i, s := range states {
		rs = append(rs, &prReviewModel.GitHubPRReview{GitHubLogin: login, ReviewState: s, SubmittedAt: at(i)})
	}
	return rs
}

// stands is the review standing for a reviewer after reviews of these states,
// in order (foldReviews, dismissed ones kept), "" for none. A state the
// decision doesn't read is never written.
func stands(states ...string) string {
	var rs []Review
	for i, s := range states {
		rs = append(rs, Review{Login: "ann", State: s, SubmittedAt: at(i)})
	}
	if f := foldReviews(rs); len(f) == 1 {
		return f[0].State
	}
	return ""
}

// decision is the task's decision from each reviewer's sequence of reviews,
// read as the task reads them: every review, folded (Decision).
func decision(reviewers ...[]string) string {
	var reviews []*prReviewModel.GitHubPRReview
	for i, states := range reviewers {
		reviews = append(reviews, reviewsOf(string(rune('a'+i)), states...)...)
	}
	slices.SortStableFunc(reviews, func(a, b *prReviewModel.GitHubPRReview) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	return Decision(reviews)
}

// A reviewer who approves and then replies on a thread (GitHub files the
// reply as a COMMENTED review) still approves (V-986).
func TestApproveThenCommentReadsApproved(t *testing.T) {
	if got := decision([]string{Approved, Commented}); got != "approved" {
		t.Errorf("approve, then comment: %q, want approved", got)
	}
	if got := stands(Approved, Commented, Commented); got != Approved {
		t.Errorf("approve, then two replies: the reviewer stands at %q, want %s", got, Approved)
	}
}

// A reviewer who asked for changes and then comments still blocks, though
// another approves.
func TestRequestChangesThenCommentReadsChangesRequested(t *testing.T) {
	if got := decision([]string{ChangesRequested, Commented}); got != "changes_requested" {
		t.Errorf("request changes, then comment: %q, want changes_requested", got)
	}
	if got := decision([]string{ChangesRequested, Commented}, []string{Approved}); got != "changes_requested" {
		t.Errorf("request changes, then comment, beside another's approval: %q, want changes_requested", got)
	}
}

// A dismissed request for changes leaves no decision; the review stays, as
// DISMISSED.
func TestRequestChangesThenDismissReadsNoDecision(t *testing.T) {
	if got := decision([]string{ChangesRequested, Dismissed}); got != "" {
		t.Errorf("request changes, then dismiss: %q, want no decision", got)
	}
	if got := stands(ChangesRequested, Dismissed); got != Dismissed {
		t.Errorf("request changes, then dismiss: the reviewer stands at %q, want %s", got, Dismissed)
	}
}

func TestAReviewersNextDecisionReplacesTheirLast(t *testing.T) {
	for _, c := range []struct {
		name   string
		states []string
		want   string
	}{
		{"comment, then approve", []string{Commented, Approved}, Approved},
		{"approve, then request changes", []string{Approved, ChangesRequested}, ChangesRequested},
		{"request changes, then approve", []string{ChangesRequested, Approved}, Approved},
		{"approve, dismissed, then comment", []string{Approved, Dismissed, Commented}, Commented},
		{"approve, then comment, then dismissed", []string{Approved, Commented, Dismissed}, Dismissed},
		{"an unsent review is no review", []string{"PENDING"}, ""},
		{"an unsent review after an approval", []string{Approved, "PENDING"}, Approved},
	} {
		if got := stands(c.states...); got != c.want {
			t.Errorf("%s: the reviewer stands at %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecisionReadsEveryReviewer(t *testing.T) {
	for _, c := range []struct {
		name  string
		state []string
		want  string
	}{
		{"no review", nil, ""},
		{"a comment", []string{Commented}, "commented"},
		{"a comment and an approval", []string{Commented, Approved}, "approved"},
		{"an approval and a comment", []string{Approved, Commented}, "approved"},
		{"an approval and a request for changes", []string{Approved, ChangesRequested}, "changes_requested"},
		{"a dismissed review alone", []string{Dismissed}, ""},
		{"a dismissed review and a comment", []string{Dismissed, Commented}, "commented"},
		{"a dismissed review and an approval", []string{Dismissed, Approved}, "approved"},
	} {
		reviews := []*prReviewModel.GitHubPRReview{}
		for _, s := range c.state {
			reviews = append(reviews, &prReviewModel.GitHubPRReview{ReviewState: s})
		}
		if got := Decision(reviews); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A batch folds into one row per reviewer, in the order each first reviewed,
// with their latest avatar and profile address; an unsent (PENDING) review is
// no row.
func TestABatchFoldsIntoOneRowPerReviewer(t *testing.T) {
	rows := foldReviews([]Review{
		{Login: "ann", AvatarURL: "a1", HTMLURL: "h1", State: "approved"},
		{Login: "bo", State: "COMMENTED"},
		{Login: "cy", State: "PENDING"},
		{Login: "ann", AvatarURL: "a2", HTMLURL: "h2", State: "COMMENTED"},
	})
	if len(rows) != 2 || rows[0].Login != "ann" || rows[1].Login != "bo" {
		t.Fatalf("rows: %+v, want ann then bo", rows)
	}
	if rows[0].State != Approved || rows[0].AvatarURL != "a2" || rows[0].HTMLURL != "h2" {
		t.Errorf("ann's row: %+v, want APPROVED with a2 and h2", rows[0])
	}
	if rows[1].State != Commented {
		t.Errorf("bo's row: %+v, want COMMENTED", rows[1])
	}
}

// The review submitted last decides, whatever order a batch arrives in
// (V-986): a batch folds as it would in the order its reviews were
// submitted, two at the same time in the order given (GitHub's, by review
// id), and the row stands for the review that decides, at its time.
func TestABatchFoldsInTheOrderItsReviewsWereSubmitted(t *testing.T) {
	for _, c := range []struct {
		name   string
		batch  []Review
		want   string
		wantAt time.Time
	}{
		{"a request for changes, then an approval submitted before it", []Review{{State: ChangesRequested, SubmittedAt: at(2)}, {State: Approved, SubmittedAt: at(1)}}, ChangesRequested, at(2)},
		{"a reply, then the approval it followed", []Review{{State: Commented, SubmittedAt: at(5)}, {State: Approved, SubmittedAt: at(1)}}, Approved, at(1)},
		{"an approval and a request for changes at the same time", []Review{{State: Approved, SubmittedAt: at(3)}, {State: ChangesRequested, SubmittedAt: at(3)}}, ChangesRequested, at(3)},
	} {
		for i := range c.batch {
			c.batch[i].Login = "ann"
		}
		rows := foldReviews(c.batch)
		if len(rows) != 1 || rows[0].State != c.want || !rows[0].SubmittedAt.Equal(c.wantAt) {
			t.Errorf("%s: folds into %v, want one row of %s at %s", c.name, rows, c.want, c.wantAt.Format(time.Kitchen))
		}
	}
}

// Standing is the row of each reviewer's review that stands, newest first:
// ann's approval through her later reply, bo's request for changes, and
// nothing of cy's, whose approval was dismissed (V-1060).
func TestStandingIsTheRowOfEachReviewersStandingReview(t *testing.T) {
	approval := &prReviewModel.GitHubPRReview{GitHubLogin: "ann", ReviewState: Approved, SubmittedAt: at(1)}
	changes := &prReviewModel.GitHubPRReview{GitHubLogin: "bo", ReviewState: ChangesRequested, SubmittedAt: at(2)}
	got := Standing([]*prReviewModel.GitHubPRReview{
		approval,
		{GitHubLogin: "cy", ReviewState: Approved, SubmittedAt: at(1)},
		changes,
		{GitHubLogin: "cy", ReviewState: Dismissed, SubmittedAt: at(3)},
		{GitHubLogin: "ann", ReviewState: Commented, SubmittedAt: at(4)},
	})
	if len(got) != 2 || got[0] != changes || got[1] != approval {
		t.Errorf("standing: %+v, want bo's request for changes, then ann's approval", got)
	}
}
