package models

// Persistence for sandbox_runs (migration 119): the per-run audit + usage
// ledger for the agent execution sandbox. One row is written per run (any
// outcome). The orchestrator reads today's usage per tier to enforce the
// sandbox budgets, and the admin run views read rows for transparency.
//
// This is a thin persistence layer: it records what it's given and sums usage;
// all policy (budgets, permission checks) lives in the business layer.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// SandboxRun mirrors a row of sandbox_runs. RunID/AgentID/ChannelID are optional
// (an assistant-initiated run has no agent/run). InputRefs is a JSON array of
// resolved input descriptors (ids/queries — never raw data).
type SandboxRun struct {
	ID            uuid.UUID  `json:"id"`
	RunID         *uuid.UUID `json:"run_id,omitempty"`
	AgentID       *uuid.UUID `json:"agent_id,omitempty"`
	ActorID       uuid.UUID  `json:"actor_id"`
	ChannelID     *uuid.UUID `json:"channel_id,omitempty"`
	CodeSHA256    string     `json:"code_sha256"`
	InputRefs     string     `json:"input_refs"` // raw JSON array
	Status        string     `json:"status"`
	WallMS        int64      `json:"wall_ms"`
	CPUMS         int64      `json:"cpu_ms"`
	PeakMemBytes  int64      `json:"peak_mem_bytes"`
	ArtifactCount int        `json:"artifact_count"`
	ArtifactBytes int64      `json:"artifact_bytes"`
	CreatedAt     time.Time  `json:"created_at"`
}

// SandboxUsage is the summed consumption for one tier over a window (today).
type SandboxUsage struct {
	Seconds int
	Runs    int
}

// RecordSandboxRun inserts one run row. Best-effort audit: a write failure is
// returned but must never block the caller's response (the caller logs + moves
// on). InputRefs must be valid JSON ("[]" when empty).
func RecordSandboxRun(ctx context.Context, r *SandboxRun) (uuid.UUID, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if r.InputRefs == "" {
		r.InputRefs = "[]"
	}
	id := uuid.New()
	const q = `
		INSERT INTO sandbox_runs
		    (id, run_id, agent_id, actor_id, channel_id, code_sha256, input_refs,
		     status, wall_ms, cpu_ms, peak_mem_bytes, artifact_count, artifact_bytes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q,
		id, r.RunID, r.AgentID, r.ActorID, r.ChannelID, r.CodeSHA256, r.InputRefs,
		r.Status, r.WallMS, r.CPUMS, r.PeakMemBytes, r.ArtifactCount, r.ArtifactBytes); err != nil {
		helpers.LogErrorWithContext(ctx, "models/RecordSandboxRun err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// sinceMidnight returns the start of the current UTC day, the window over which
// daily budgets are summed (matches how the token budgets reset daily).
func sinceMidnight() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// sumUsage runs a SUM(wall_ms)/COUNT query with the given WHERE fragment/arg and
// returns seconds (rounded up) + run count for today. Central helper so every
// tier query is identical.
func sumUsage(ctx context.Context, whereCol string, arg interface{}) (SandboxUsage, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT COALESCE(SUM(wall_ms),0), COUNT(*) FROM sandbox_runs
	      WHERE ` + whereCol + ` = $1 AND created_at >= $2`
	var wallMS int64
	var runs int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, arg, sinceMidnight()).Scan(&wallMS, &runs); err != nil {
		return SandboxUsage{}, err
	}
	return SandboxUsage{Seconds: msToSecondsCeil(wallMS), Runs: runs}, nil
}

// AgentSandboxUsageToday sums today's sandbox usage for one agent.
func AgentSandboxUsageToday(ctx context.Context, agentID uuid.UUID) (SandboxUsage, error) {
	return sumUsage(ctx, "agent_id", agentID)
}

// ChannelSandboxUsageToday sums today's sandbox usage in one channel.
func ChannelSandboxUsageToday(ctx context.Context, channelID uuid.UUID) (SandboxUsage, error) {
	return sumUsage(ctx, "channel_id", channelID)
}

// WorkspaceSandboxUsageToday sums today's sandbox usage across the whole
// workspace (single-tenant, so no tenant filter).
func WorkspaceSandboxUsageToday(ctx context.Context) (SandboxUsage, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT COALESCE(SUM(wall_ms),0), COUNT(*) FROM sandbox_runs WHERE created_at >= $1`
	var wallMS int64
	var runs int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, sinceMidnight()).Scan(&wallMS, &runs); err != nil {
		return SandboxUsage{}, err
	}
	return SandboxUsage{Seconds: msToSecondsCeil(wallMS), Runs: runs}, nil
}

// msToSecondsCeil converts milliseconds to whole seconds, rounding UP so budget
// accounting never under-counts a partial second.
func msToSecondsCeil(ms int64) int {
	if ms <= 0 {
		return 0
	}
	return int((ms + 999) / 1000)
}
