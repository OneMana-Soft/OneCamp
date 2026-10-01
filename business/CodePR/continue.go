package codepr

// Continuing the agent's own pull request.
//
// People iterate on an agent's pull request the way they iterate on a
// colleague's: "also handle the empty list", a review comment, a reply in the
// thread. Each follow-up used to run as a fresh coding job that opened a
// second pull request beside the first, so the reviewer had two half-changes
// and the conversation split in two.
//
// Now a follow-up on a thread or task whose agent already opened a pull
// request pushes a new commit to that pull request's branch. It does so only
// when it is safe, checked with GitHub by the identity that will push:
//   - the pull request is still open (a merged or closed one gets a new PR),
//   - its head is the branch the agent created (onecamp-agent/…), and
//   - that branch lives in the same repository (never a fork).
//
// The runner never force-pushes, so if someone else pushed to the branch while
// the run worked, their commits are kept and the run says so.

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// AgentBranchPrefix starts every branch the agent creates (see BuildHeadBranch).
// Only such branches are ever continued: a person's branch is theirs.
const AgentBranchPrefix = "onecamp-agent/"

// PriorPR is the pull request a follow-up would continue, as recorded when the
// agent opened it.
type PriorPR struct {
	URL        string  `json:"url"`
	Repo       RepoRef `json:"repo"`
	HeadBranch string  `json:"head_branch"`
	BaseBranch string  `json:"base_branch"`
}

// PRHead is GitHub's current answer about a pull request.
type PRHead struct {
	Found    bool
	Open     bool
	HeadRef  string
	HeadRepo string // owner/name
	BaseRef  string
}

// PRChecker reads a pull request as the given credential sees it.
type PRChecker func(ctx context.Context, cred Credential, repo RepoRef, number int) (PRHead, error)

var prURLRe = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)/?$`)

// ParsePRURL splits a github.com pull request URL. Pure.
func ParsePRURL(url string) (RepoRef, int, bool) {
	m := prURLRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return RepoRef{}, 0, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return RepoRef{}, 0, false
	}
	return RepoRef{Owner: m[1], Name: m[2]}, n, true
}

// continueRefusal says why a prior pull request can't be continued, or ""
// when it can. The reason is shown to the person, since they asked for a
// change to that pull request and are getting a new one instead. Pure.
func continueRefusal(p PriorPR, h PRHead) string {
	switch {
	case !strings.HasPrefix(p.HeadBranch, AgentBranchPrefix):
		return "its branch is not one I created"
	case !h.Found:
		return "I can no longer see it"
	case !h.Open:
		return "it is already merged or closed"
	case !strings.EqualFold(h.HeadRepo, p.Repo.FullName()):
		return "its branch is not in " + p.Repo.FullName()
	case h.HeadRef != p.HeadBranch:
		return "its branch changed"
	}
	return ""
}

// resolveContinuation decides whether this run continues task.Continue. It
// returns the pull request to continue (nil for a fresh one) and, when a
// prior pull request was set aside, a note saying why. Never pushes to a
// pull request it could not check.
func (o *Orchestrator) resolveContinuation(ctx context.Context, task Task, cred Credential) (*PriorPR, int, string) {
	p := task.Continue
	if p == nil {
		return nil, 0, ""
	}
	repo, number, ok := ParsePRURL(p.URL)
	if !ok || !strings.EqualFold(repo.FullName(), p.Repo.FullName()) {
		return nil, 0, ""
	}
	label := "pull request #" + strconv.Itoa(number)
	if o.CheckPR == nil {
		return nil, 0, "I couldn't check " + label + ", so I opened a new one."
	}
	h, err := o.CheckPR(ctx, cred, p.Repo, number)
	if err != nil {
		return nil, 0, "I couldn't check " + label + ", so I opened a new one."
	}
	if why := continueRefusal(*p, h); why != "" {
		return nil, 0, "I opened a new pull request because " + why + " (" + label + ")."
	}
	cont := *p
	if b := strings.TrimSpace(h.BaseRef); b != "" {
		cont.BaseBranch = b
	}
	return &cont, number, ""
}

// continuedOutcome reports a follow-up pushed to an existing pull request.
func (o *Orchestrator) continuedOutcome(task Task, prior PriorPR, number int, result CodingResult, verdict Verdict, status string) Outcome {
	out := Outcome{
		Status:     status,
		Repo:       task.Repo,
		PRURL:      prior.URL,
		HeadBranch: prior.HeadBranch,
		Verdict:    verdict,
		Verifier:   result.Verifier,
		DiffStat:   result.DiffStat,
		Usage:      result.Usage,
		Continued:  true,
	}
	out.Message = continuedMessage(prior.URL, number, status, verdict, result.Verifier)
	o.record(context.Background(), task, out, len(result.CandidateList))
	return out
}

// continuedMessage is the one honest line for a follow-up commit. Pure.
func continuedMessage(url string, number int, status string, verdict Verdict, vr VerifierReport) string {
	label := "pull request #" + strconv.Itoa(number)
	var b strings.Builder
	switch status {
	case StatusOK:
		b.WriteString("Pushed a follow-up commit to " + label + ": " + url)
		if vr.AllPassed && len(vr.Ran) > 0 {
			if vr.HadTests {
				b.WriteString(". Build and tests pass.")
			} else {
				b.WriteString(". Build passes (no tests present).")
			}
		}
	case StatusTimeout:
		b.WriteString("I ran out of time, so I pushed the work I had to " + label + " for review: " + url)
	default:
		b.WriteString("I pushed my attempt to " + label + ", but the build or tests don't pass yet: " + url)
	}
	if !verdict.InScope && strings.TrimSpace(verdict.Concern) != "" {
		b.WriteString(" Heads up: " + strings.TrimRight(verdict.Concern, ".") + ".")
	}
	return b.String()
}

// ContinuationInstruction is the task a follow-up run works on: the original
// request, then what the teammate asked for since, so the run knows what the
// pull request is for and what to change now. Pure.
func ContinuationInstruction(original, followup string) string {
	original, followup = strings.TrimSpace(original), strings.TrimSpace(followup)
	switch {
	case original == "" || original == followup:
		return followup
	case followup == "":
		return original
	}
	return original + "\n\nThe work above is already done. Now make only this follow-up change:\n" + followup
}

// withNote appends a note to an outcome's message.
func withNote(out Outcome, note string) Outcome {
	if note = strings.TrimSpace(note); note != "" && out.Status == StatusOK {
		out.Message = strings.TrimSpace(out.Message + " " + note)
	}
	return out
}
