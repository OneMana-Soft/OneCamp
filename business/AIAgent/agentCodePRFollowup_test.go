package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	codepr "github.com/akashc777/OneCamp/business/CodePR"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func stubFollowUpLookups(t *testing.T, firstID uuid.UUID, original string, pr *aiModels.OpenedCodePR, prErr error) {
	t.Helper()
	oldFirst, oldPR := firstJobForSource, openedPRForSource
	t.Cleanup(func() { firstJobForSource, openedPRForSource = oldFirst, oldPR })
	firstJobForSource = func(context.Context, uuid.UUID, string, string) (uuid.UUID, string, bool, error) {
		return firstID, original, firstID != uuid.Nil, nil
	}
	openedPRForSource = func(context.Context, uuid.UUID, string, string) (*aiModels.OpenedCodePR, error) {
		return pr, prErr
	}
}

func TestFollowUpOnCodePR_PointsAtTheThreadsPullRequest(t *testing.T) {
	job := &model.AgentTask{Id: uuid.New(), AgentId: uuid.New(), SourceType: "code_pr", SourceId: "post:1", Prompt: "Also the margin"}
	stubFollowUpLookups(t, uuid.New(), "Fix the padding", &aiModels.OpenedCodePR{
		PRURL: "https://github.com/acme/svc/pull/7", RepoOwner: "acme", RepoName: "svc", HeadBranch: "onecamp-agent/fix-1", BaseBranch: "main",
	}, nil)
	task := codepr.Task{Instruction: job.Prompt}
	followUpOnCodePR(context.Background(), job, &task)

	if !strings.HasPrefix(task.Instruction, "Fix the padding") || !strings.HasSuffix(task.Instruction, "Also the margin") {
		t.Fatalf("the run must know the original request and the follow-up: %q", task.Instruction)
	}
	c := task.Continue
	if c == nil || c.URL != "https://github.com/acme/svc/pull/7" || c.Repo.FullName() != "acme/svc" || c.HeadBranch != "onecamp-agent/fix-1" || c.BaseBranch != "main" {
		t.Fatalf("continue: %+v", c)
	}
}

func TestFollowUpOnCodePR_LeavesTheFirstJobAlone(t *testing.T) {
	job := &model.AgentTask{Id: uuid.New(), Prompt: "Fix the padding"}
	stubFollowUpLookups(t, job.Id, "Fix the padding", &aiModels.OpenedCodePR{PRURL: "https://github.com/acme/svc/pull/7"}, nil)
	task := codepr.Task{Instruction: job.Prompt}
	followUpOnCodePR(context.Background(), job, &task)
	if task.Instruction != "Fix the padding" || task.Continue != nil {
		t.Fatalf("the first job is not a follow-up: %+v", task)
	}
}

func TestFollowUpOnCodePR_WithoutAPullRequestStillKnowsTheOriginal(t *testing.T) {
	for name, err := range map[string]error{"none": nil, "lookup failed": errors.New("db down")} {
		job := &model.AgentTask{Id: uuid.New(), Prompt: "Try again"}
		stubFollowUpLookups(t, uuid.New(), "Fix the padding", nil, err)
		task := codepr.Task{Instruction: job.Prompt}
		followUpOnCodePR(context.Background(), job, &task)
		if task.Continue != nil || !strings.HasPrefix(task.Instruction, "Fix the padding") {
			t.Fatalf("%s: %+v", name, task)
		}
	}
}
