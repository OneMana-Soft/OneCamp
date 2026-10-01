package codepr

// Live wiring — assembling a ready-to-run Orchestrator from the workspace's live
// configuration and the real collaborators (runner client, budget ledger, scope
// judge, GitHub PR write, audit ledger). This is the single seam the tool
// executor and the durable worker call; everything upstream is injected and
// unit-tested, so this file is the (thin, gated) glue that binds them to the
// datastore + sidecar + GitHub. It never runs anything itself — Orchestrator.Run
// does, and only when the feature is enabled.
//
// All configuration is read from the already-fetched AISettings so the caller
// controls one DB read; the runner token is decrypted here and never returned.

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/google/uuid"
)

// LiveDeps carries what NewLiveOrchestrator needs beyond the global settings: the
// acting agent's per-agent daily caps (0 = none) and a per-run unique id for the
// fresh branch. Settings is the singleton AI config (already fetched).
type LiveDeps struct {
	Settings          *aiModels.AISettings
	AgentDailyMinutes int
	AgentDailyRuns    int
	RunID             string
}

// NewLiveOrchestrator builds an Orchestrator wired to the live runner, budget
// ledger, scope judge, GitHub PR write, and audit ledger, configured from
// settings. It is safe to build even when the feature is disabled or the runner
// is unconfigured — the runner then reports StatusUnavailable, so a run degrades
// cleanly rather than panicking. Returns nil only when settings is nil.
func NewLiveOrchestrator(deps LiveDeps) *Orchestrator {
	s := deps.Settings
	if s == nil {
		return nil
	}

	// Decrypt the runner token (best-effort; an empty token just means the
	// sidecar isn't token-protected or is misconfigured — the runner still
	// reports unavailable on a bad endpoint).
	token := ""
	if len(s.CodePRRunnerTokenEnc) > 0 {
		if dec, err := aiModels.DecryptAPIKey(s.CodePRRunnerTokenEnc); err == nil {
			token = dec
		}
	}
	runner := NewHTTPCodingRunner(HTTPRunnerConfig{
		BaseURL: s.CodePRRunnerURL,
		Token:   token,
		// The sidecar calls back to this main server's internal coding-LLM proxy
		// for completions (model-agnostic, metered). The URL is the server's
		// INTERNAL (in-cluster) address, and the proxy shares the runner token.
		LLMProxyURL:   codePRLLMProxyURL(),
		LLMProxyToken: token,
	})

	policy := PolicyFlagOpen
	if strings.TrimSpace(s.CodePROutOfScopePolicy) == string(PolicyPause) {
		policy = PolicyPause
	}

	// Whether the agent may act on ANY repo the connected GitHub account can
	// reach (not only ones linked to a project). The admin setting is the source
	// of truth (default OFF — linked-only, the blast-radius-safe default); the
	// AI_CODE_PR_ALLOW_UNLINKED env var, when set, overrides it at boot for
	// automated deployments.
	allowUnlinked := codePRAllowUnlinked(s.CodePRAllowUnlinked)

	// How long ONE coding run may work. The admin setting is the source of truth
	// (0 = use the built-in default; the AI_CODE_PR_WALL_MINUTES env var is the
	// boot-time escape hatch), and publishing it here refreshes the package-level
	// cache that CodingWall / MaxRunWallClock read from synchronous timing paths
	// — so the durable queue's lease TTL and the transport deadline track the
	// same configured limit the runner is handed below. Always clamped.
	wall := SetConfiguredCodingWall(s.CodePRWallMinutes)

	// Snapshot the caps so the budget closure doesn't re-read settings per call.
	wsMin, wsRuns := s.CodePRWorkspaceDailyMinutes, s.CodePRWorkspaceDailyRuns
	chMin, chRuns := s.CodePRChannelDailyMinutes, s.CodePRChannelDailyRuns
	agentMin, agentRuns := deps.AgentDailyMinutes, deps.AgentDailyRuns

	return &Orchestrator{
		Runner: runner,
		Resolve: func(ctx context.Context, instruction string, cred Credential) RepoResolution {
			// Access is checked as the ACTING identity, not as the workspace admin.
			// Otherwise an unlinked repository could be cleared because the admin's
			// account can see it, and then worked on by someone who cannot.
			return ResolveRepo(ctx, instruction, allowUnlinked, credentialRepoAccess(cred))
		},
		Judge: JudgeChangeScope,
		PlanSelectors: func(ctx context.Context, instruction string) []Selector {
			s, _ := AuthorSelectors(ctx, instruction)
			return s
		},
		Steering:    liveSteering,
		Policy:      policy,
		DraftOnRed:  s.CodePRDraftOnRed,
		EgressHosts: parseEgressHosts(s.CodePREgressAllowlist),
		Limits:      defaultCodingLimits().withWall(wall),
		RunID:       deps.RunID,
		MintToken:   liveMintCredential,
		OpenPR:      liveOpenPR,
		CheckPR:     liveCheckPR,
		Budget:      makeBudgetLookup(agentMin, agentRuns, chMin, chRuns, wsMin, wsRuns),
		Record:      makeRecorder(),
	}
}

// codePRLLMProxyURL is the address the coding RUNNER uses to call back to this
// server's model-agnostic LLM proxy (POST /internal/code-run/llm). It is an
// internal, in-cluster URL — never public. Operators can override it with the
// AI_CODE_PR_LLM_PROXY_URL env var (e.g. a different service name or port); when
// unset it defaults to the conventional address used by every shipped compose
// file (the go-service container on :3000), so the standard deployment works
// with zero extra configuration instead of silently reporting "unavailable".
func codePRLLMProxyURL() string {
	if v := strings.TrimSpace(os.Getenv("AI_CODE_PR_LLM_PROXY_URL")); v != "" {
		return v
	}
	return "http://go-service:3000/internal/code-run/llm"
}

// codePRAllowUnlinked resolves whether the agent may work on ANY repository the
// connected GitHub account can reach, not only ones linked to a project. The
// admin setting (settingValue) is the source of truth; the
// AI_CODE_PR_ALLOW_UNLINKED env var, when set to a parseable bool, overrides it
// at boot (an escape hatch for automated deployments). Unset env => the setting.
func codePRAllowUnlinked(settingValue bool) bool {
	if v := strings.TrimSpace(os.Getenv("AI_CODE_PR_ALLOW_UNLINKED")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return settingValue
}

// liveCredentialSources binds the credential lookups to the real per-user
// connector and a repo-access check made with THAT SAME token.
//
// Note what is deliberately absent: the workspace admin integration
// (githubBusiness.GitHubAccessToken). It is an admin's personal OAuth token with
// "repo" scope across every organisation they belong to, granted to import issues
// and sync tasks. Pushing code with it would credit that admin as the author of
// code they never wrote, hand every member who can task the agent the admin's
// full GitHub reach, and couple issue sync to coding runs. See credential.go.
func liveCredentialSources() CredentialSources {
	return CredentialSources{
		UserToken: connectorBusiness.GitHubTokenForUser,
		UserCanPushToRepo: func(ctx context.Context, token string, repo RepoRef) (RepoWriteCheck, error) {
			a, err := githubBusiness.RepoPushAccessWithToken(ctx, token, repo.Owner, repo.Name)
			if err != nil {
				return RepoWriteCheck{}, err
			}
			return RepoWriteCheck{
				Visible:       a.Visible,
				CanPush:       a.CanPush,
				Archived:      a.Archived,
				DefaultBranch: a.DefaultBranch,
			}, nil
		},
	}
}

// liveMintCredential resolves the identity a run pushes with: the requesting
// person's own GitHub connector, verified against the target repository, or
// nothing at all (which the orchestrator turns into an actionable block).
func liveMintCredential(ctx context.Context, task Task, repo RepoRef) (Credential, error) {
	return ChooseCredential(ctx, task, repo, liveCredentialSources()), nil
}

// credentialRepoAccess adapts a resolved Credential into the RepoAccessChecker
// repo resolution needs, so the question "can this run reach that repository" is
// asked of the identity that will push it. An unusable credential answers "no
// access" for everything rather than falling through to a broader identity.
func credentialRepoAccess(cred Credential) RepoAccessChecker {
	return func(ctx context.Context, owner, name string) (bool, error) {
		if !cred.Usable() {
			return false, nil
		}
		a, err := githubBusiness.RepoPushAccessWithToken(ctx, cred.Token, owner, name)
		if err != nil {
			return false, err
		}
		// Resolution asks whether the run could work here at all, so it uses the
		// same writability bar the push will hit. Accepting a read-only repository
		// here would only defer the refusal until after a full run had been spent.
		return a.Writable(), nil
	}
}

// liveOpenPR opens the PR via the GitHub write path, mapping the GitHub
// already-exists sentinel to the orchestrator's so it's surfaced as success-ish.
func liveOpenPR(ctx context.Context, repo RepoRef, head, base, title, body string, draft bool) (string, error) {
	res, err := githubBusiness.CreatePullRequest(ctx, repo.Owner, repo.Name, head, base, title, body, draft)
	if err != nil {
		if err == githubBusiness.ErrPullRequestExists {
			return "", ErrPullRequestExists
		}
		return "", err
	}
	return res.HTMLURL, nil
}

// liveCheckPR reads a pull request with the run's own credential, so "may I
// push to this branch" is answered by the identity that will push.
func liveCheckPR(ctx context.Context, cred Credential, repo RepoRef, number int) (PRHead, error) {
	if !cred.Usable() {
		return PRHead{}, nil
	}
	h, err := githubBusiness.PullRequestHeadWithToken(ctx, cred.Token, repo.Owner, repo.Name, number)
	if err != nil {
		return PRHead{}, err
	}
	return PRHead{Found: h.Found, Open: h.Open, HeadRef: h.HeadRef, HeadRepo: h.HeadRepo, BaseRef: h.BaseRef}, nil
}

// liveSteering distills learned guidance for a repo from its own recent
// code_pr_runs outcomes (merge / scope / verify / draft rates) and renders it as
// a prompt preamble. Best-effort: a read error or thin history yields "" (the
// prompt is unchanged), so a new repo or a DB hiccup never blocks a run.
func liveSteering(ctx context.Context, repo RepoRef) string {
	if !repo.Valid() {
		return ""
	}
	rows, err := aiModels.ListCodePRRunSignalsByRepo(ctx, repo.Owner, repo.Name, 0)
	if err != nil || len(rows) == 0 {
		return ""
	}
	signals := make([]RunSignal, 0, len(rows))
	for _, r := range rows {
		signals = append(signals, SignalFromRun(r.Status, r.PRURL, r.AllPassed, r.HadTests, r.InScope, r.Draft, PROutcome(r.Outcome)))
	}
	return FormatSteeringBlock(DistillSteering(Aggregate(signals)))
}

// makeBudgetLookup builds the BudgetLookup closure: it reads today's usage per
// tier from the code_pr_runs ledger (keyed by the task's agent/channel) and
// combines it with the configured caps. Usage-read failures degrade to zero
// (best-effort — the orchestrator treats a Budget error as "proceed").
func makeBudgetLookup(agentMin, agentRuns, chMin, chRuns, wsMin, wsRuns int) BudgetLookup {
	return func(ctx context.Context, task Task) (BudgetInput, error) {
		var agentUsedMin, agentUsedRuns, chUsedMin, chUsedRuns, wsUsedMin, wsUsedRuns int

		if task.AgentID != uuid.Nil {
			if u, err := aiModels.AgentCodePRUsageToday(ctx, task.AgentID); err == nil {
				agentUsedMin, agentUsedRuns = u.Minutes, u.Runs
			}
		}
		if chID := strings.TrimSpace(task.Surface.ChannelID); chID != "" {
			if cu, perr := uuid.Parse(chID); perr == nil {
				if u, err := aiModels.ChannelCodePRUsageToday(ctx, cu); err == nil {
					chUsedMin, chUsedRuns = u.Minutes, u.Runs
				}
			}
		}
		if u, err := aiModels.WorkspaceCodePRUsageToday(ctx); err == nil {
			wsUsedMin, wsUsedRuns = u.Minutes, u.Runs
		}

		return BuildBudgetInput(
			agentMin, agentRuns, agentUsedMin, agentUsedRuns,
			chMin, chRuns, chUsedMin, chUsedRuns,
			wsMin, wsRuns, wsUsedMin, wsUsedRuns,
		), nil
	}
}

// makeRecorder builds the RunRecorder closure that writes one code_pr_runs audit
// row per terminal outcome. Best-effort: a write failure is logged, never fatal.
func makeRecorder() RunRecorder {
	return func(ctx context.Context, task Task, out Outcome, candidateCount int) {
		row := &aiModels.CodePRRun{
			ActorID:          task.OwnerUserID,
			RepoOwner:        out.Repo.Owner,
			RepoName:         out.Repo.Name,
			BaseBranch:       task.BaseBranch,
			HeadBranch:       out.HeadBranch,
			WholeRepo:        task.WholeRepo,
			CandidateCount:   candidateCount,
			DiffFiles:        out.DiffStat.Files,
			DiffAdded:        out.DiffStat.Added,
			DiffRemoved:      out.DiffStat.Removed,
			PartialScope:     out.DiffStat.PartialScope,
			Verifier:         marshalJSON(out.Verifier),
			HadTests:         out.Verifier.HadTests,
			AllPassed:        out.Verifier.AllPassed,
			JudgeVerdict:     marshalJSON(out.Verdict),
			InScope:          out.Verdict.InScope,
			Draft:            out.Draft,
			Status:           out.Status,
			WallMS:           out.Usage.WallMS,
			CPUMS:            out.Usage.CPUMS,
			PeakMemBytes:     out.Usage.PeakMemBytes,
			VerifyIterations: out.Usage.VerifyIterations,
			PRURL:            out.PRURL,
			Message:          out.Message,
			Surface:          marshalJSON(task.Surface),
		}
		if task.AgentID != uuid.Nil {
			id := task.AgentID
			row.AgentID = &id
		}
		if task.AgentTaskID != uuid.Nil {
			id := task.AgentTaskID
			row.AgentTaskID = &id
		}
		if chID := strings.TrimSpace(task.Surface.ChannelID); chID != "" {
			if cu, perr := uuid.Parse(chID); perr == nil {
				row.ChannelID = &cu
			}
		}
		if _, err := aiModels.RecordCodePRRun(ctx, row); err != nil {
			helpers.LogErrorWithContext(ctx, "codepr: record run failed: %v", err)
		}
	}
}

// parseEgressHosts decodes the stored raw-JSON host array into a slice. Returns
// nil on empty/invalid input (default-deny with no allowed hosts).
func parseEgressHosts(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var hosts []string
	if err := json.Unmarshal([]byte(raw), &hosts); err != nil {
		return nil
	}
	return hosts
}

// marshalJSON compacts a value to a JSON string, or "{}" on failure — so an
// audit row always has valid JSON in its jsonb columns.
func marshalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
