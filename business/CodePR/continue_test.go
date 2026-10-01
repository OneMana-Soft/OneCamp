package codepr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const priorURL = "https://github.com/o/r/pull/7"

func priorPR() *PriorPR {
	return &PriorPR{URL: priorURL, Repo: RepoRef{Owner: "o", Name: "r"}, HeadBranch: "onecamp-agent/fix-padding-abc", BaseBranch: "beta"}
}

func openHead() PRHead {
	return PRHead{Found: true, Open: true, HeadRef: "onecamp-agent/fix-padding-abc", HeadRepo: "o/r", BaseRef: "beta"}
}

// continuingOrch is baseOrch whose PR opener fails the test: a continued run
// must never open a second pull request.
func continuingOrch(t *testing.T, runner CodingRunner, head PRHead, headErr error) (*Orchestrator, *[]Outcome, *int) {
	o, rec := baseOrch(runner)
	opened := 0
	o.OpenPR = func(_ context.Context, _ RepoRef, _, _, _, _ string, _ bool) (string, error) {
		opened++
		return "https://github.com/o/r/pull/8", nil
	}
	o.Resolve = func(_ context.Context, _ string, _ Credential) RepoResolution {
		t.Fatal("a continued run must not re-resolve its repository")
		return RepoResolution{}
	}
	o.CheckPR = func(_ context.Context, cred Credential, repo RepoRef, number int) (PRHead, error) {
		if cred.Token != "tok" || repo.FullName() != "o/r" || number != 7 {
			t.Fatalf("checked the wrong thing: %+v %v %d", cred, repo, number)
		}
		return head, headErr
	}
	return o, rec, &opened
}

func continuingTask() Task {
	tk := task()
	tk.BaseBranch = ""
	tk.Continue = priorPR()
	return tk
}

func TestRun_FollowUpPushesToTheOpenPR(t *testing.T) {
	m := &mockCodingRunner{Result: okResult()}
	o, rec, opened := continuingOrch(t, m, openHead(), nil)
	out := o.Run(context.Background(), continuingTask())

	if !m.LastJob.ContinueBranch || m.LastJob.HeadBranch != "onecamp-agent/fix-padding-abc" || m.LastJob.BaseBranch != "beta" {
		t.Fatalf("the runner must continue the PR's branch: %+v", m.LastJob)
	}
	if *opened != 0 {
		t.Fatal("a follow-up must not open a second pull request")
	}
	if out.Status != StatusOK || !out.Continued || out.PRURL != priorURL {
		t.Fatalf("want a continued outcome on the prior PR, got %+v", out)
	}
	if !strings.Contains(out.Message, "Pushed a follow-up commit to pull request #7") || !strings.Contains(out.Message, "tests pass") {
		t.Fatalf("message: %q", out.Message)
	}
	if len(*rec) != 1 || (*rec)[0].PRURL != priorURL {
		t.Fatalf("the run must be recorded against the prior PR: %+v", *rec)
	}
}

func TestRun_FollowUpOpensANewPRWhenThePriorOneCannotTakeIt(t *testing.T) {
	cases := map[string]struct {
		head PRHead
		err  error
		note string
	}{
		"merged":         {PRHead{Found: true, Open: false, HeadRef: "onecamp-agent/fix-padding-abc", HeadRepo: "o/r"}, nil, "already merged or closed"},
		"gone":           {PRHead{}, nil, "can no longer see it"},
		"fork":           {PRHead{Found: true, Open: true, HeadRef: "onecamp-agent/fix-padding-abc", HeadRepo: "someone/r"}, nil, "not in o/r"},
		"branch changed": {PRHead{Found: true, Open: true, HeadRef: "other", HeadRepo: "o/r"}, nil, "branch changed"},
		"check failed":   {PRHead{}, errors.New("github down"), "couldn't check pull request #7"},
	}
	for name, tc := range cases {
		m := &mockCodingRunner{Result: okResult()}
		o, _, opened := continuingOrch(t, m, tc.head, tc.err)
		out := o.Run(context.Background(), continuingTask())
		if m.LastJob.ContinueBranch || m.LastJob.HeadBranch == "onecamp-agent/fix-padding-abc" {
			t.Errorf("%s: must start a fresh branch: %+v", name, m.LastJob)
		}
		if *opened != 1 || out.Continued || out.PRURL != "https://github.com/o/r/pull/8" {
			t.Errorf("%s: want a new PR, got %+v", name, out)
		}
		if !strings.Contains(out.Message, tc.note) {
			t.Errorf("%s: the message must say why a new PR was opened: %q", name, out.Message)
		}
	}
}

func TestRun_FollowUpNeverPushesWithoutAChecker(t *testing.T) {
	m := &mockCodingRunner{Result: okResult()}
	o, _, _ := continuingOrch(t, m, openHead(), nil)
	o.CheckPR = nil
	o.Run(context.Background(), continuingTask())
	if m.LastJob.ContinueBranch {
		t.Fatal("an unchecked pull request must never be pushed to")
	}
}

func TestRun_FollowUpNeverContinuesAPersonsBranch(t *testing.T) {
	m := &mockCodingRunner{Result: okResult()}
	head := openHead()
	head.HeadRef = "akash/feature"
	o, _, _ := continuingOrch(t, m, head, nil)
	tk := continuingTask()
	tk.Continue.HeadBranch = "akash/feature"
	out := o.Run(context.Background(), tk)
	if m.LastJob.ContinueBranch || !strings.Contains(out.Message, "not one I created") {
		t.Fatalf("a person's branch is theirs: job %+v, message %q", m.LastJob, out.Message)
	}
}

func TestRun_FollowUpThatDoesNotGoGreenStaysOnThePR(t *testing.T) {
	for _, status := range []string{StatusNoGreen, StatusTimeout} {
		res := okResult()
		res.Status = status
		res.HeadBranch = "onecamp-agent/fix-padding-abc"
		res.Verifier.AllPassed = false
		o, _, opened := continuingOrch(t, &mockCodingRunner{Result: res}, openHead(), nil)
		o.DraftOnRed = true
		out := o.Run(context.Background(), continuingTask())
		if *opened != 0 || out.Status != status || out.PRURL != priorURL || !out.Continued {
			t.Fatalf("%s: want the attempt reported on the prior PR, got %+v", status, out)
		}
		if !strings.Contains(out.Message, "pull request #7") {
			t.Fatalf("%s: message %q", status, out.Message)
		}
	}
}

func TestRun_FollowUpPushRefusedIsReportedHonestly(t *testing.T) {
	res := CodingResult{Status: StatusError, Message: "Someone pushed to the pull request's branch while I worked, so I did not push over their commits."}
	o, _, opened := continuingOrch(t, &mockCodingRunner{Result: res}, openHead(), nil)
	out := o.Run(context.Background(), continuingTask())
	if *opened != 0 || out.Status != StatusError || !strings.Contains(out.Message, "did not push over their commits") {
		t.Fatalf("got %+v", out)
	}
}

func TestParsePRURL(t *testing.T) {
	repo, n, ok := ParsePRURL(" https://github.com/acme/svc.go/pull/42 ")
	if !ok || repo.FullName() != "acme/svc.go" || n != 42 {
		t.Fatalf("got %v %d %v", repo, n, ok)
	}
	for _, bad := range []string{"", "https://github.com/acme/svc/issues/4", "https://gitlab.com/a/b/pull/1", "https://github.com/a/b/pull/0", "https://github.com/a/b/pull/1/files"} {
		if _, _, ok := ParsePRURL(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestContinuationInstruction(t *testing.T) {
	if got := ContinuationInstruction("Fix the padding", "Also the margin"); !strings.HasPrefix(got, "Fix the padding\n") || !strings.HasSuffix(got, "Also the margin") {
		t.Fatalf("got %q", got)
	}
	if got := ContinuationInstruction("", "Also the margin"); got != "Also the margin" {
		t.Fatalf("no original: %q", got)
	}
	if got := ContinuationInstruction("Same", "Same"); got != "Same" {
		t.Fatalf("same text: %q", got)
	}
}
