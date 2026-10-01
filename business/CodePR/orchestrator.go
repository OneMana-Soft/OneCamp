package codepr

// Orchestrator — the trusted decision layer that turns a coding Task into an
// outcome (a PR, a needs_human pause, or an honest failure). It is the keystone
// that ties together repo resolution, budget gating, the coding runner, the
// scope judge, PR authoring, and audit — WITHOUT knowing how any of them are
// implemented. Every collaborator is injected (interface or func), so the whole
// flow is proven with fakes/a mock runner before any real clone/edit/push
// exists, and the real wiring (the tool + durable trigger) just supplies
// concrete implementations.
//
// Invariants enforced here (see .kiro/specs/agent-code-pr): never guess a repo
// (resolve or ask), never start over budget, never open a non-draft PR without
// verified green, honor the out-of-scope policy, always fresh branch, and always
// an honest, sanitized user-facing message.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// OutOfScopePolicy governs what happens when the scope judge flags drift.
type OutOfScopePolicy string

const (
	PolicyFlagOpen OutOfScopePolicy = "flag_open" // open the PR flagged with the concern (default)
	PolicyPause    OutOfScopePolicy = "pause"     // pause via needs_human instead of opening
)

// ErrPullRequestExists lets the OpenPR collaborator signal "a PR already exists"
// so the orchestrator surfaces it as success-ish rather than a hard failure. The
// GitHub layer returns its own sentinel; the wiring maps it to this.
var ErrPullRequestExists = errors.New("a pull request already exists for this branch")

// Outcome is the result of a coding run, ready to render as an in-thread result
// card + a durable task terminal state. Status is one of the Status* codes;
// StopReason carries a budget/typed stop code when relevant.
type Outcome struct {
	Status     string
	StopReason string
	Repo       RepoRef
	PRURL      string
	HeadBranch string
	// BranchURL / PRCreateURL are set when a branch was pushed but no PR link is
	// available (PR-open failed, a PR already exists, or a WIP branch was left on
	// a no-green run), so the user always gets a clickable way to open/review the
	// change instead of a dead end.
	BranchURL          string
	PRCreateURL        string
	Draft              bool
	Verdict            Verdict
	Verifier           VerifierReport
	DiffStat           DiffStat
	Usage              CodingUsage
	Message            string // honest, sanitized, user-facing summary
	NeedsHumanQuestion string // set when Status == StatusBlocked and a human must answer
	// Continued: the run pushed a commit to the agent's existing pull request
	// (PRURL) instead of opening a new one.
	Continued bool
	// Credential records WHICH GitHub identity the run acted with. Carried on the
	// outcome so the audit row and the pull-request body can both disclose it —
	// "was this done as the person who asked, or as the shared workspace
	// connection?" is not answerable after the fact otherwise.
	Credential CredentialKind
}

// BudgetLookup returns the caps + usage for a task's tiers (agent/channel/
// workspace). Injected so the orchestrator stays DB-free and testable.
type BudgetLookup func(ctx context.Context, task Task) (BudgetInput, error)

// CredentialMinter resolves WHICH GitHub identity the run acts with (see
// credential.go) for a given task and repo, and never logs the token. It takes
// the task because the choice depends on the run's principal: a run a person
// asked for should act as that person, while a scheduled run has no person to act
// as and must fall back to the workspace credential and disclose it.
// Injected; may be nil when the runner supplies its own auth.
type CredentialMinter func(ctx context.Context, task Task, repo RepoRef) (Credential, error)

// JudgeFunc runs the scope judge over a task + diff. Injected (defaults to
// JudgeChangeScope) so tests can force verdicts.
type JudgeFunc func(ctx context.Context, task, diff string) (Verdict, error)

// PROpener opens a PR and returns its URL. Injected (wired to
// githubBusiness.CreatePullRequest + the PR builders). Returns
// ErrPullRequestExists when one already exists.
type PROpener func(ctx context.Context, repo RepoRef, head, base, title, body string, draft bool) (string, error)

// RunRecorder persists the audit/usage row. Best-effort (a failure never blocks
// the outcome). Injected (wired to aiModels.RecordCodePRRun).
type RunRecorder func(ctx context.Context, task Task, out Outcome, candidateCount int)

// Orchestrator wires the injected collaborators + policy. Any nil collaborator
// degrades safely (see Run). RunID is a per-run uniqueness source for the fresh
// head branch.
type Orchestrator struct {
	Runner CodingRunner
	// Resolve picks the target repository. It receives the run's CREDENTIAL because
	// deciding "may this run work on that repository" has to be answered for the
	// identity that will actually push — resolving against one identity's access
	// and pushing with another's is an authorisation check that proves nothing.
	Resolve   func(ctx context.Context, instruction string, cred Credential) RepoResolution
	Budget    BudgetLookup
	MintToken CredentialMinter
	Judge     JudgeFunc
	// PlanSelectors authors the deterministic whole-repo selectors for a
	// size-agnostic task (injected; wired to AuthorSelectors). Nil => whole-repo
	// tasks run without selectors (the runner falls back to its own retrieval),
	// so the orchestrator stays testable without the AI service.
	PlanSelectors func(ctx context.Context, instruction string) []Selector
	// Steering returns a learned-guidance block (from this repo's past review
	// outcomes) to prepend to the coding prompt, or "" when there's nothing to
	// add. Injected + nil-safe so Run stays testable and the no-history path is
	// unchanged.
	Steering func(ctx context.Context, repo RepoRef) string
	OpenPR   PROpener
	// CheckPR confirms a prior pull request is still open on the agent's branch
	// before a follow-up pushes to it. Nil => follow-ups always open a new one.
	CheckPR    PRChecker
	Record     RunRecorder
	Policy     OutOfScopePolicy
	DraftOnRed bool
	// OnStage, when set, hears each stage as it starts (see stages.go), so the
	// person waiting sees progress instead of minutes of silence.
	OnStage     func(StageUpdate)
	EgressHosts []string
	Limits      CodingLimits
	RunID       string
}

// Run executes one coding task end-to-end and returns an Outcome. It never
// returns an error: every failure mode maps to a typed Outcome with an honest
// message, so a durable caller can render it and set the right terminal state.
func (o *Orchestrator) Run(ctx context.Context, task Task) (outcome Outcome) {
	// The identity this run pushes with, stamped onto WHICHEVER outcome the run
	// returns. Done with a named return and a defer because Run exits from a dozen
	// places, and "which GitHub identity did this act as" must be recorded on every
	// one of them — an audit row that answers it for some outcomes and not others
	// is not an audit trail.
	var cred Credential
	defer func() {
		if outcome.Credential == CredentialNone {
			outcome.Credential = cred.Kind
		}
	}()

	if o.Resolve == nil || o.Runner == nil {
		return Outcome{Status: StatusError, Message: "Code PRs are not fully configured."}
	}

	// 1. Establish the identity FIRST, before anything is resolved or run. The
	// repository is not known yet, so this is provisional: it settles whether there
	// is a usable identity at all. Doing it first is what lets resolution be
	// authorised against the same identity that will push, instead of the run being
	// cleared by one account's access and executed with another's.
	if o.MintToken != nil {
		c, err := o.MintToken(ctx, task, RepoRef{})
		if err != nil || !c.Usable() {
			return Outcome{
				Status:             StatusBlocked,
				Credential:         CredentialNone,
				NeedsHumanQuestion: c.BlockedMessage(),
				Message:            "No GitHub identity available to push with.",
			}
		}
		cred = c
	}

	// 2. Resolve the target repo (never guess; ask on ambiguity), asking about
	// access as the acting identity.
	// A follow-up on the agent's own pull request already knows its repository;
	// re-resolving it from the follow-up's words could only pick another.
	if task.Continue != nil && task.Continue.Repo.Valid() {
		task.Repo = task.Continue.Repo
	} else {
		task.Continue = nil
		res := o.Resolve(ctx, task.Instruction, cred)
		if !res.Found {
			return o.blockedForRepo(res)
		}
		task.Repo = res.Resolved
	}

	// 3. Bind the identity to the RESOLVED repository: confirm this person can
	// actually push there. Provisional selection above only proved they have a
	// GitHub connection at all.
	if o.MintToken != nil {
		c, err := o.MintToken(ctx, task, task.Repo)
		if err != nil || !c.Usable() {
			return Outcome{
				Status:             StatusBlocked,
				Repo:               task.Repo,
				Credential:         CredentialNone,
				NeedsHumanQuestion: c.BlockedMessage(),
				Message:            "No GitHub identity available to push with.",
			}
		}
		cred = c
	}

	// Continue the agent's open pull request, or say why a new one is opened.
	prior, prNumber, freshNote := o.resolveContinuation(ctx, task, cred)
	task.Continue = prior
	if prior != nil && strings.TrimSpace(task.BaseBranch) == "" {
		task.BaseBranch = prior.BaseBranch
	}
	defer func() { outcome = withNote(outcome, freshNote) }()

	// 4. Budget gate (best-effort: a usage-lookup failure does not block).
	if o.Budget != nil {
		if bi, err := o.Budget(ctx, task); err == nil {
			if d := CheckBudget(bi); !d.Allowed {
				return Outcome{
					Status:     StatusBlocked,
					StopReason: d.StopReason,
					Repo:       task.Repo,
					Message:    d.Message,
				}
			}
		}
	}

	// 5. Build the job (fresh branch, credential, egress, limits).
	limits := o.Limits
	if limits.Wall == 0 {
		limits = defaultCodingLimits()
	}

	// Whole-repo tasks are made SIZE-AGNOSTIC by authoring deterministic
	// selectors: the runner executes them to enumerate a finite candidate set +
	// bounded shards, instead of loading a giant repo into the model. Best-effort
	// — no selectors just means the runner uses its own retrieval.
	var selectors []Selector
	if task.WholeRepo && o.PlanSelectors != nil {
		selectors = SanitizeSelectors(o.PlanSelectors(ctx, task.Instruction))
	}

	// Learned steering: prepend guidance distilled from this repo's own past
	// review outcomes so the run adapts to how this team actually reviews.
	// Best-effort + nil-safe: no history ⇒ prompt is unchanged.
	prompt := task.Instruction
	if o.Steering != nil {
		if block := strings.TrimSpace(o.Steering(ctx, task.Repo)); block != "" {
			prompt = block + "\n\n" + prompt
		}
	}

	job := CodingJob{
		ID:         o.RunID,
		Repo:       task.Repo,
		CloneToken: cred.Token,
		BaseBranch: strings.TrimSpace(task.BaseBranch),
		HeadBranch: BuildHeadBranch(task.Instruction, o.RunID),
		Prompt:     prompt,
		Selectors:  selectors,
		Limits:     limits,
		Egress:     EgressPolicy{AllowHosts: o.EgressHosts},
	}
	if prior != nil {
		job.HeadBranch, job.ContinueBranch = prior.HeadBranch, true
	}

	// 6. Execute in the (isolated) runner.
	o.stage(StageWorking, task.Repo, nil)
	result, err := o.Runner.Run(ctx, job)
	if err != nil {
		return Outcome{Status: StatusError, Repo: task.Repo,
			Message: "The coding run could not complete: " + Sanitize(err.Error())}
	}

	// 7. Map non-success runner statuses to honest outcomes.
	if result.Status != StatusOK {
		if prior != nil && strings.TrimSpace(result.HeadBranch) != "" &&
			(result.Status == StatusNoGreen || result.Status == StatusTimeout) {
			// The attempt is already on the pull request's branch; opening a PR
			// for it would only fail as a duplicate.
			return o.continuedOutcome(task, *prior, prNumber, result, Verdict{InScope: true}, result.Status)
		}
		return o.mapNonOK(task, result)
	}

	// 8. Scope judge (fails open on error).
	o.stage(StageChecking, task.Repo, &result)
	verdict := Verdict{InScope: true}
	if o.Judge != nil && strings.TrimSpace(result.Diff) != "" {
		if v, jerr := o.Judge(ctx, task.Instruction, result.Diff); jerr == nil {
			verdict = v
		}
	}
	if !verdict.InScope && o.policy() == PolicyPause {
		out := Outcome{
			Status:             StatusBlocked,
			Repo:               task.Repo,
			Verdict:            verdict,
			Verifier:           result.Verifier,
			DiffStat:           result.DiffStat,
			Usage:              result.Usage,
			NeedsHumanQuestion: "The change may go beyond the task: " + verdict.Concern + ". Want me to open it anyway, or adjust?",
			Message:            "Paused for review — the change may exceed the task's scope.",
		}
		o.record(ctx, task, out, len(result.CandidateList))
		return out
	}

	// 9. Open the PR (draft when it was not verified green and policy allows),
	// or, continuing, report the commit on the one that exists.
	if prior != nil {
		return o.continuedOutcome(task, *prior, prNumber, result, verdict, StatusOK)
	}
	o.stage(StageOpening, task.Repo, &result)
	return o.openPR(ctx, task, result, verdict, false)
}

// mapNonOK turns a non-ok runner result into an honest outcome. A no-green run
// may still open a DRAFT PR when the admin policy allows and a branch was pushed.
func (o *Orchestrator) mapNonOK(task Task, result CodingResult) Outcome {
	msg := strings.TrimSpace(Sanitize(result.Message))
	switch result.Status {
	case StatusNoGreen:
		if o.DraftOnRed && strings.TrimSpace(result.HeadBranch) != "" {
			return o.openPRFromResult(task, result, Verdict{InScope: true}, true)
		}
		if msg == "" {
			msg = "I couldn't get the change to a passing build/tests, so I didn't open a PR."
		}
		return o.pushedWithoutPR(task, result, StatusNoGreen, msg)
	case StatusTooLarge:
		if msg == "" {
			msg = task.Repo.FullName() + " is too large to work within the configured limits."
		}
		return o.finalNonOK(task, result, msg)
	case StatusBlocked:
		return Outcome{Status: StatusBlocked, Repo: task.Repo, Usage: result.Usage,
			NeedsHumanQuestion: msg, Message: msg}
	case StatusTimeout:
		// A run can now exhaust its wall AND still have pushed the work it
		// finished (the runner reserves wrap-up time for exactly that). When a
		// branch exists, treat it like a no-green run: a draft PR under the admin
		// policy, otherwise a branch + one-click PR link. "Out of time" must never
		// mean "your work was thrown away".
		if strings.TrimSpace(result.HeadBranch) != "" {
			if o.DraftOnRed {
				return o.openPRFromResult(task, result, Verdict{InScope: true}, true)
			}
			if msg == "" {
				msg = "I ran out of time before finishing, so I pushed the work I had instead of opening a PR."
			}
			return o.pushedWithoutPR(task, result, StatusTimeout, msg)
		}
		if msg == "" {
			msg = "The coding run hit its time limit before finishing."
		}
		return o.finalNonOK(task, result, msg)
	case StatusUnavailable:
		if msg == "" {
			msg = "The coding runner is currently unavailable."
		}
		return o.finalNonOK(task, result, msg)
	default: // StatusError / unknown
		if msg == "" {
			msg = "The coding run failed."
		}
		return o.finalNonOK(task, result, msg)
	}
}

// pushedWithoutPR renders the outcome for a run that pushed a branch but is not
// opening a PR itself — a no-green run without the draft policy, or a run that ran
// out of time. Shared by both so the branch-link logic exists once: the user
// always gets the branch plus a one-click "open the PR" link instead of a dead
// end. Falls back to the plain honest message when no branch was pushed.
func (o *Orchestrator) pushedWithoutPR(task Task, result CodingResult, status, msg string) Outcome {
	out := Outcome{Status: status, Repo: task.Repo, Verifier: result.Verifier,
		DiffStat: result.DiffStat, Usage: result.Usage, Message: msg}
	if head := strings.TrimSpace(result.HeadBranch); head != "" {
		out.HeadBranch = head
		out.BranchURL = task.Repo.BranchURL(head)
		out.PRCreateURL = task.Repo.PRCreateURL(task.BaseBranch, head)
		if out.PRCreateURL != "" {
			out.Message = msg + " The work is on a branch if you want to review it or open the PR yourself: " + out.PRCreateURL
		}
	}
	o.record(context.Background(), task, out, len(result.CandidateList))
	return out
}

func (o *Orchestrator) finalNonOK(task Task, result CodingResult, msg string) Outcome {
	out := Outcome{Status: result.Status, Repo: task.Repo, Usage: result.Usage,
		Verifier: result.Verifier, DiffStat: result.DiffStat, Message: msg}
	if out.Status == "" {
		out.Status = StatusError
	}
	o.record(context.Background(), task, out, len(result.CandidateList))
	return out
}

// openPR opens the PR for a successful (or flagged) run.
func (o *Orchestrator) openPR(ctx context.Context, task Task, result CodingResult, verdict Verdict, draft bool) Outcome {
	return o.openPRFromResult(task, result, verdict, draft)
}

// openPRFromResult builds the title/body and opens the PR, mapping the result to
// an outcome. Records the run (best-effort) regardless of PR success.
func (o *Orchestrator) openPRFromResult(task Task, result CodingResult, verdict Verdict, draft bool) Outcome {
	ctx := context.Background()
	title := BuildPRTitle(task.Instruction)
	body := BuildPRBody(PRProvenance{
		AgentName:      task.AgentDisplayName,
		RequestedBy:    task.RequestedByName,
		Task:           task.Instruction,
		Repo:           task.Repo,
		DiffStat:       result.DiffStat,
		Verifier:       result.Verifier,
		Verdict:        verdict,
		Draft:          draft,
		WholeRepo:      task.WholeRepo,
		CandidateCount: len(result.CandidateList),
	})

	out := Outcome{
		Status:     StatusOK,
		Repo:       task.Repo,
		HeadBranch: result.HeadBranch,
		Draft:      draft,
		Verdict:    verdict,
		Verifier:   result.Verifier,
		DiffStat:   result.DiffStat,
		Usage:      result.Usage,
	}

	if o.OpenPR == nil {
		out.Status = StatusError
		out.Message = "Code PRs are not fully configured (no PR opener)."
		o.record(ctx, task, out, len(result.CandidateList))
		return out
	}
	base := strings.TrimSpace(task.BaseBranch)
	// Precompute link fallbacks from the pushed branch so no failure path leaves
	// the user without a way to see/open the change.
	out.BranchURL = task.Repo.BranchURL(result.HeadBranch)
	out.PRCreateURL = task.Repo.PRCreateURL(base, result.HeadBranch)
	url, err := o.OpenPR(ctx, task.Repo, result.HeadBranch, base, title, body, draft)
	if err != nil {
		if errors.Is(err, ErrPullRequestExists) {
			out.Message = "A pull request already exists for this change on " + task.Repo.FullName() + "."
			if out.BranchURL != "" {
				out.Message += " Branch: " + out.BranchURL
			}
			o.record(ctx, task, out, len(result.CandidateList))
			return out
		}
		out.Status = StatusError
		out.Message = "I made the change and pushed the branch, but couldn't open the pull request automatically: " + Sanitize(err.Error())
		if out.PRCreateURL != "" {
			out.Message += " You can open it in one click here: " + out.PRCreateURL
		}
		o.record(ctx, task, out, len(result.CandidateList))
		return out
	}
	out.PRURL = url
	// The PR now exists; the branch/create fallbacks are redundant noise.
	out.BranchURL = ""
	out.PRCreateURL = ""
	out.Message = prSuccessMessage(url, draft, verdict, result.Verifier)
	o.record(ctx, task, out, len(result.CandidateList))
	return out
}

// prSuccessMessage renders the honest one-line result for the thread.
func prSuccessMessage(url string, draft bool, verdict Verdict, vr VerifierReport) string {
	var b strings.Builder
	if draft {
		b.WriteString("Opened a draft pull request (couldn't fully verify): ")
	} else {
		b.WriteString("Opened a pull request: ")
	}
	b.WriteString(url)
	if vr.AllPassed && len(vr.Ran) > 0 {
		if vr.HadTests {
			b.WriteString(" — build and tests pass.")
		} else {
			b.WriteString(" — build passes (no tests present).")
		}
	}
	if !verdict.InScope && strings.TrimSpace(verdict.Concern) != "" {
		fmt.Fprintf(&b, " Heads up: %s.", strings.TrimRight(verdict.Concern, "."))
	}
	return b.String()
}

// blockedForRepo turns an unresolved repo into a needs_human outcome.
func (o *Orchestrator) blockedForRepo(res RepoResolution) Outcome {
	if res.UnlinkedDisabled.Valid() {
		msg := "You asked me to work on " + res.UnlinkedDisabled.FullName() +
			", but it isn't linked to a project and I'm currently limited to linked repositories. Link it in the project's GitHub settings, or an admin can turn on \"work on any repository the agent can access\" in AI settings — then ask me again."
		return Outcome{Status: StatusBlocked, NeedsHumanQuestion: msg, Message: msg, Repo: res.UnlinkedDisabled}
	}
	if res.InaccessibleRepo.Valid() {
		msg := "I couldn't access " + res.InaccessibleRepo.FullName() +
			" with this workspace's connected GitHub account. Double-check the owner/name, or connect an account (or token) that has access to it — then ask me again."
		return Outcome{Status: StatusBlocked, NeedsHumanQuestion: msg, Message: msg, Repo: res.InaccessibleRepo}
	}
	if res.NoneLinked {
		msg := "No GitHub repository is connected to this workspace yet. Link one in the project's GitHub settings (or name a repo I have access to, like owner/name) and I'll take it from there."
		return Outcome{Status: StatusBlocked, NeedsHumanQuestion: msg, Message: msg}
	}
	names := CandidateNames(res.Candidates)
	q := "Which repository should I work on? " + strings.Join(names, ", ")
	return Outcome{Status: StatusBlocked, NeedsHumanQuestion: q, Message: q, Repo: firstRepo(res.Candidates)}
}

func firstRepo(cands []RepoRef) RepoRef {
	if len(cands) > 0 {
		return cands[0]
	}
	return RepoRef{}
}

func (o *Orchestrator) policy() OutOfScopePolicy {
	if o.Policy == PolicyPause {
		return PolicyPause
	}
	return PolicyFlagOpen
}

func (o *Orchestrator) record(ctx context.Context, task Task, out Outcome, candidateCount int) {
	if o.Record != nil {
		o.Record(ctx, task, out, candidateCount)
	}
}
