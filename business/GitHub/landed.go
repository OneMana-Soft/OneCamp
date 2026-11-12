package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	githubTaskActivityDomain "github.com/akashc777/OneCamp/domain/GitHubTaskActivity"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	githubTaskActivityModel "github.com/akashc777/OneCamp/models/postgres/GitHubTaskActivity"
	"github.com/google/uuid"
)

// Landed is a change that landed for a task (V-670): a pull request from the
// task's branch that passed its checks, was approved and merged, and its
// commit naming the task by its key ("Fixes Q4-8") on the default branch.
type Landed struct {
	Repo     string // owner/name
	Number   int    // the pull request's
	Title    string // the pull request's
	Branch   string // the pull request's head
	Author   string // the login that opened the pull request and pushed the commit
	Reviewer string // the login that approved it
	Checks   string // the checks' conclusion, as a check run says it ("success")
	SHA      string // the commit's
	Message  string // the commit's, naming the task
}

// RecordLanded writes on a task of project what GitHub's deliveries of l
// leave there, through the rows and writers the handlers use: the pull
// request's fields, state and checks (handlePullRequestEvent opened and
// closed, handleCheckRunEvent), its review (handlePullRequestReviewEvent,
// updateTaskReviewState), the lines of the task's GitHub activity, the commit
// (handlePushEvent) and the close by its closing word, as the integration's
// bot (moveAsBot, as closeByRef with no link: done). It is for a change no
// repository delivered, the demo's (business/DemoSeed): no link, token or call
// to GitHub, and nothing goes out on the event bus (no AI review, no
// outgoing webhook). Each part is written once: a line the task's activity
// has already is not written again, the fields and the review are set in
// place, and a task already done is left as it is.
func RecordLanded(ctx context.Context, task, project uuid.UUID, l Landed) error {
	had, err := githubTaskActivityDomain.GetGitHubTaskActivitiesByTaskID(ctx, task)
	if err != nil {
		return fmt.Errorf("reading the task's GitHub activity: %w", err)
	}
	has := map[string]bool{}
	for _, a := range had {
		has[a.ActivityType] = true
	}
	var history []githubTaskActivityModel.GitHubTaskActivity
	line := func(kind, login string, title, body *string) {
		if !has[kind] {
			history = append(history, githubTaskActivityModel.GitHubTaskActivity{TaskID: task, ActivityType: kind,
				GitHubLogin: &login, Title: title, Body: body})
		}
	}
	base := "https://github.com/" + l.Repo
	prURL, commitURL := fmt.Sprintf("%s/pull/%d", base, l.Number), base+"/commit/"+l.SHA
	const review = "APPROVED" // as a review delivery says it, upper-cased

	var errs []error
	if err := taskDomain.SetGitHubPRFieldsOnTask(ctx, task, l.Number, prURL, l.Branch); err != nil {
		errs = append(errs, fmt.Errorf("the pull request: %w", err))
	}
	if err := taskDomain.UpdateTaskPRState(ctx, task, "merged", l.Checks, "", boolPtr(false)); err != nil {
		errs = append(errs, fmt.Errorf("the pull request's state: %w", err))
	}
	if err := githubPRReviewDomain.UpsertGitHubPRReview(ctx, task, l.Reviewer, "", "", review, time.Now()); err != nil {
		errs = append(errs, fmt.Errorf("the review: %w", err))
	}
	updateTaskReviewState(ctx, task)
	line("pr_opened", l.Author, &l.Title, nil)
	line("pr_review_"+strings.ToLower(review), l.Reviewer, nil, nil)
	line("pr_merged", l.Author, &l.Title, nil)
	line("commit_linked", l.Author, &l.Message, &commitURL)
	if err := githubTaskActivityDomain.CreateGitHubTaskActivities(ctx, history); err != nil {
		errs = append(errs, fmt.Errorf("the task's GitHub activity: %w", err))
	}
	to, err := taskStatusBusiness.Resolve(ctx, project.String(), closeTarget(nil))
	if err == nil {
		err = moveAsBot(ctx, task, project, to, closeTrigger)
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("the close: %w", err))
	}
	return errors.Join(errs...)
}
