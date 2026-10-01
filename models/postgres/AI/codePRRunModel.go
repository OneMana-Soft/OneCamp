package models

// Persistence for code_pr_runs (migration 123): the per-run audit + usage ledger
// for the agent code-PR feature. One row is written per coding run (any
// outcome). The orchestrator reads today's usage per tier to enforce the coding
// budgets, and the admin/run views read rows for transparency + PR provenance.
//
// Thin persistence layer: it records what it's given and sums usage; all policy
// (budgets, permission checks, scope-judge) lives in the business layer.

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// CodePRRun mirrors a row of code_pr_runs. RunID/AgentID/ChannelID are optional
// (an assistant/API-initiated run has no agent/run). Selectors/Verifier/
// JudgeVerdict are raw JSON (compact audit artifacts); the full diff lives
// behind DiffRef, never duplicated here.
type CodePRRun struct {
	ID               uuid.UUID  `json:"id"`
	RunID            *uuid.UUID `json:"run_id,omitempty"`
	AgentID          *uuid.UUID `json:"agent_id,omitempty"`
	ActorID          uuid.UUID  `json:"actor_id"`
	ChannelID        *uuid.UUID `json:"channel_id,omitempty"`
	RepoOwner        string     `json:"repo_owner"`
	RepoName         string     `json:"repo_name"`
	BaseBranch       string     `json:"base_branch"`
	HeadBranch       string     `json:"head_branch"`
	WholeRepo        bool       `json:"whole_repo"`
	Selectors        string     `json:"selectors"` // raw JSON array
	CandidateCount   int        `json:"candidate_count"`
	DiffRef          string     `json:"diff_ref"`
	DiffFiles        int        `json:"diff_files"`
	DiffAdded        int        `json:"diff_added"`
	DiffRemoved      int        `json:"diff_removed"`
	PartialScope     bool       `json:"partial_scope"`
	Verifier         string     `json:"verifier"` // raw JSON object
	HadTests         bool       `json:"had_tests"`
	AllPassed        bool       `json:"all_passed"`
	InScope          bool       `json:"in_scope"`      // scope judge did not flag drift (migration 125)
	Draft            bool       `json:"draft"`         // opened as a draft (migration 125)
	JudgeVerdict     string     `json:"judge_verdict"` // raw JSON object
	Status           string     `json:"status"`
	WallMS           int64      `json:"wall_ms"`
	CPUMS            int64      `json:"cpu_ms"`
	PeakMemBytes     int64      `json:"peak_mem_bytes"`
	VerifyIterations int        `json:"verify_iterations"`
	PRURL            string     `json:"pr_url"`
	Message          string     `json:"message"`
	// Surface is the raw JSON reply-surface descriptor (business/AIAgent Surface
	// shape) the run posted to — where a later PR comment/review continues the
	// agent's work. Empty for older rows / non-conversational surfaces.
	Surface   string    `json:"surface,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Outcome is the PR's terminal state (migration 124), captured best-effort
	// by the GitHub webhook: "" (unknown/open), "merged", "merged_with_edits",
	// or "closed_unmerged". OutcomeAt is when it was observed (nil until then).
	Outcome   string     `json:"outcome"`
	OutcomeAt *time.Time `json:"outcome_at,omitempty"`
	// AgentTaskID is the durable agent job that made this run (migration 168),
	// so a follow-up job can find the pull request its thread already has.
	AgentTaskID *uuid.UUID `json:"agent_task_id,omitempty"`
}

// CodePRUsage is the summed consumption for one tier over today's window.
type CodePRUsage struct {
	Minutes int
	Runs    int
}

// RecordCodePRRun inserts one run row. Best-effort audit: a write failure is
// returned but must never block the caller's response. JSON columns default to
// valid empty JSON when unset.
func RecordCodePRRun(ctx context.Context, r *CodePRRun) (uuid.UUID, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if r.Selectors == "" {
		r.Selectors = "[]"
	}
	if r.Verifier == "" {
		r.Verifier = "{}"
	}
	if r.JudgeVerdict == "" {
		r.JudgeVerdict = "{}"
	}
	id := uuid.New()
	const q = `
		INSERT INTO code_pr_runs
		    (id, run_id, agent_id, actor_id, channel_id, repo_owner, repo_name,
		     base_branch, head_branch, whole_repo, selectors, candidate_count,
		     diff_ref, diff_files, diff_added, diff_removed, partial_scope,
		     verifier, had_tests, all_passed, judge_verdict, status,
		     wall_ms, cpu_ms, peak_mem_bytes, verify_iterations, pr_url, message,
		     in_scope, draft, surface, agent_task_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13,$14,$15,$16,$17,
		        $18::jsonb,$19,$20,$21::jsonb,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)`
	var surfaceArg interface{}
	if strings.TrimSpace(r.Surface) != "" {
		surfaceArg = r.Surface
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q,
		id, r.RunID, r.AgentID, r.ActorID, r.ChannelID, r.RepoOwner, r.RepoName,
		r.BaseBranch, r.HeadBranch, r.WholeRepo, r.Selectors, r.CandidateCount,
		r.DiffRef, r.DiffFiles, r.DiffAdded, r.DiffRemoved, r.PartialScope,
		r.Verifier, r.HadTests, r.AllPassed, r.JudgeVerdict, r.Status,
		r.WallMS, r.CPUMS, r.PeakMemBytes, r.VerifyIterations, r.PRURL, r.Message,
		r.InScope, r.Draft, surfaceArg, r.AgentTaskID); err != nil {
		helpers.LogErrorWithContext(ctx, "models/RecordCodePRRun err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetCodePRRunSurfaceByPRURL returns the reply surface + originating agent of the
// most recent code-PR run that opened the given PR, so a comment/review on that
// PR can be routed back to the OneCamp thread and the agent's work continued.
// found is false (no error) when no agent run opened this PR — the common case
// for a human PR, so the caller's webhook stays a cheap best-effort probe. A
// blank surface means the run had no conversational surface (e.g. an
// assistant/API trigger) and cannot be continued in-thread.
func GetCodePRRunSurfaceByPRURL(ctx context.Context, prURL string) (agentID *uuid.UUID, surface string, found bool, err error) {
	prURL = strings.TrimSpace(prURL)
	if prURL == "" {
		return nil, "", false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT agent_id, COALESCE(surface::text, '')
		FROM code_pr_runs
		WHERE pr_url = $1
		ORDER BY created_at DESC
		LIMIT 1`
	var aid uuid.NullUUID
	var surf string
	scanErr := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, prURL).Scan(&aid, &surf)
	if scanErr == sql.ErrNoRows {
		return nil, "", false, nil
	}
	if scanErr != nil {
		helpers.LogErrorWithContext(ctx, "models/GetCodePRRunSurfaceByPRURL err: %+v", scanErr)
		return nil, "", false, scanErr
	}
	if aid.Valid {
		id := aid.UUID
		agentID = &id
	}
	return agentID, surf, true, nil
}

// OpenedCodePR is a pull request an agent's coding job opened: where it lives
// and the branch it was opened from.
type OpenedCodePR struct {
	PRURL      string
	RepoOwner  string
	RepoName   string
	HeadBranch string
	BaseBranch string
}

// LatestOpenedCodePRForSource returns the newest pull request the agent's jobs
// for one source (the same thread or task, keyed like ai_agent_tasks) opened
// or pushed to, or nil when there is none. It says nothing about whether the
// pull request is still open; the caller asks GitHub.
func LatestOpenedCodePRForSource(ctx context.Context, agentID uuid.UUID, sourceType, sourceID string) (*OpenedCodePR, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT r.pr_url, r.repo_owner, r.repo_name, r.head_branch, r.base_branch
		FROM code_pr_runs r
		JOIN ai_agent_tasks t ON t.id = r.agent_task_id
		WHERE t.agent_id = $1 AND t.source_type = $2 AND t.source_id = $3
		  AND r.pr_url <> '' AND r.head_branch <> '' AND r.status = 'ok'
		ORDER BY r.created_at DESC
		LIMIT 1`
	var p OpenedCodePR
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, agentID, sourceType, sourceID).
		Scan(&p.PRURL, &p.RepoOwner, &p.RepoName, &p.HeadBranch, &p.BaseBranch)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/LatestOpenedCodePRForSource err: %+v", err)
		return nil, err
	}
	return &p, nil
}

// sumCodePRUsage runs a SUM(wall_ms)/COUNT query with the given WHERE
// fragment/arg and returns minutes (rounded up) + run count for today. Central
// helper so every tier query is identical. Reuses sinceMidnight() /
// msToMinutesCeil.
func sumCodePRUsage(ctx context.Context, whereCol string, arg interface{}) (CodePRUsage, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT COALESCE(SUM(wall_ms),0), COUNT(*) FROM code_pr_runs
	      WHERE ` + whereCol + ` = $1 AND created_at >= $2`
	var wallMS int64
	var runs int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, arg, sinceMidnight()).Scan(&wallMS, &runs); err != nil {
		return CodePRUsage{}, err
	}
	return CodePRUsage{Minutes: msToMinutesCeil(wallMS), Runs: runs}, nil
}

// AgentCodePRUsageToday sums today's coding usage for one agent.
func AgentCodePRUsageToday(ctx context.Context, agentID uuid.UUID) (CodePRUsage, error) {
	return sumCodePRUsage(ctx, "agent_id", agentID)
}

// ChannelCodePRUsageToday sums today's coding usage in one channel.
func ChannelCodePRUsageToday(ctx context.Context, channelID uuid.UUID) (CodePRUsage, error) {
	return sumCodePRUsage(ctx, "channel_id", channelID)
}

// WorkspaceCodePRUsageToday sums today's coding usage across the whole workspace
// (single-tenant, so no tenant filter).
func WorkspaceCodePRUsageToday(ctx context.Context) (CodePRUsage, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT COALESCE(SUM(wall_ms),0), COUNT(*) FROM code_pr_runs WHERE created_at >= $1`
	var wallMS int64
	var runs int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, sinceMidnight()).Scan(&wallMS, &runs); err != nil {
		return CodePRUsage{}, err
	}
	return CodePRUsage{Minutes: msToMinutesCeil(wallMS), Runs: runs}, nil
}

// SetCodePROutcomeByPRURL records the terminal outcome of an agent-opened PR,
// matched by its pr_url (the durable key written when the run opened the PR).
// Returns the number of run rows updated (0 when no agent run opened that PR —
// the common case for human PRs, so the webhook stays a cheap best-effort probe).
//
// Idempotent + monotonic: it never overwrites an already-merged outcome (a
// merge is terminal ground truth), so a later "closed"/reopen webhook can't
// downgrade a merged run. A blank pr_url or outcome is a no-op. Any run sharing
// the pr_url is updated so re-runs against the same PR stay consistent.
func SetCodePROutcomeByPRURL(ctx context.Context, prURL, outcome string) (int64, error) {
	if prURL == "" || outcome == "" {
		return 0, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		UPDATE code_pr_runs
		   SET outcome = $2, outcome_at = NOW()
		 WHERE pr_url = $1
		   AND outcome NOT IN ('merged', 'merged_with_edits')
		   AND outcome IS DISTINCT FROM $2`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, prURL, outcome)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetCodePROutcomeByPRURL err: %+v", err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CodePRRunSignal is the compact, read-only projection the quality scorecard
// needs from a run row — exactly codepr.RunSignal's inputs, without importing
// the business layer into the model. Kept lean so the reliability view can scan
// many runs cheaply off the promoted columns (migration 125).
type CodePRRunSignal struct {
	Status    string
	PRURL     string
	AllPassed bool
	HadTests  bool
	InScope   bool
	Draft     bool
	Outcome   string
}

// ExecCodePRRunSignals runs a pre-built scorecard-signal query (see
// domain/CodePR, which owns the optional since-window filter) and scans the
// read-only projection. Shared by the time-window and per-repo signal reads.
func ExecCodePRRunSignals(ctx context.Context, query string, args []interface{}) ([]CodePRRunSignal, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CodePRRunSignal, 0, 64)
	for rows.Next() {
		var s CodePRRunSignal
		if err := rows.Scan(&s.Status, &s.PRURL, &s.AllPassed, &s.HadTests, &s.InScope, &s.Draft, &s.Outcome); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CodePRRunRow is a compact ledger row for the admin runs list (transparency:
// what the coding agent did and how it ended). Distinct from the scorecard
// signal projection — this carries display fields, not just rates inputs.
type CodePRRunRow struct {
	ID        uuid.UUID
	RepoOwner string
	RepoName  string
	Status    string
	Outcome   string
	PRURL     string
	Draft     bool
	AllPassed bool
	DiffFiles int
	Message   string
	CreatedAt time.Time
}

// ListRecentCodePRRuns returns the most recent coding runs (newest first),
// capped at limit (<=0 => 50, max 500), for the admin runs view. Read-only +
// bounded (uses idx_code_pr_runs_created).
func ListRecentCodePRRuns(ctx context.Context, limit int) ([]CodePRRunRow, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 500 {
		limit = 50
	}
	const q = `SELECT id, repo_owner, repo_name, status, outcome, pr_url, draft,
	       all_passed, diff_files, message, created_at
	       FROM code_pr_runs
	       ORDER BY created_at DESC LIMIT $1`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CodePRRunRow, 0, 64)
	for rows.Next() {
		var r CodePRRunRow
		if err := rows.Scan(&r.ID, &r.RepoOwner, &r.RepoName, &r.Status, &r.Outcome,
			&r.PRURL, &r.Draft, &r.AllPassed, &r.DiffFiles, &r.Message, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListCodePRRunSignalsByRepo returns the scorecard signals for one repository
// (owner/name), newest first, capped at limit (<=0 => 200). Powers per-repo
// learned steering: distilling how a specific repo's PRs actually fare. Uses the
// idx_code_pr_runs_created index; the repo filter keeps the scan small.
func ListCodePRRunSignalsByRepo(ctx context.Context, owner, name string, limit int) ([]CodePRRunSignal, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 2000 {
		limit = 200
	}

	const q = `SELECT status, pr_url, all_passed, had_tests, in_scope, draft, outcome
	        FROM code_pr_runs
	       WHERE repo_owner = $1 AND repo_name = $2
	       ORDER BY created_at DESC LIMIT $3`
	return ExecCodePRRunSignals(ctx, q, []interface{}{owner, name, limit})
}

// msToMinutesCeil converts milliseconds to whole minutes, rounding UP so budget
// accounting never under-counts a partial minute.
func msToMinutesCeil(ms int64) int {
	if ms <= 0 {
		return 0
	}
	return int((ms + 59_999) / 60_000)
}
