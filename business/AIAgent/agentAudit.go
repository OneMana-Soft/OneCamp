package business

// An agent run in the tamper-evident log.
//
// The run ledger already recorded what an agent did, in its own table, for the
// product's own use. The admin audit log, which is the hash-chained record a
// reviewer or an auditor reads, had nothing from the internal runner at all:
// only agents reaching in over MCP appeared there. So a workspace could answer
// "what did this agent do" and could not answer "show me everything agents did,
// and prove the list has not been edited", which is the question enterprise
// buyers have started asking specifically.
//
// Deliberately one row per RUN rather than per tool call. A run is the unit a
// person authorised and the unit they would review; per-call rows would bury
// the reviewable event under its own mechanics, and the per-call detail already
// exists on the run record this row names.

import (
	"context"
	"fmt"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// recordRunInAuditLog writes one entry for a finished run.
//
// Best effort, and that is a deliberate difference from the MCP path. There the
// audit write gates the call: no record, no action, because the action has not
// happened yet. Here the work is already done and the run row is already
// written, so failing the run to punish a logging fault would destroy work to
// record it. The failure is logged loudly instead.
// finishedRun is everything the two records of a completed run need to know
// about it.
//
// One struct rather than two long positional parameter lists. The audit row and
// the span describe the SAME event and want almost the same facts, and a call
// taking three adjacent []string arguments is a silent swap waiting to happen:
// it compiles, it runs, and the failed tools appear in the succeeded column
// forever. Named fields make that mistake unwritable.
type finishedRun struct {
	Agent         *model.AiAgent
	RunID         uuid.UUID
	TriggerSource string
	Status        string
	Error         string
	// ToolsSucceeded and FailedTools partition what actually executed, by name.
	ToolsSucceeded []string
	FailedTools    []string
	// GovernanceBlocks names the refusals: tool calls the model asked for and
	// policy declined. Not failures — the opposite, the system working.
	GovernanceBlocks []string
	StartedAt        time.Time
	EndedAt          time.Time
	DryRun           bool
}

func recordRunInAuditLog(ctx context.Context, r finishedRun) {
	agent := r.Agent
	if agent == nil {
		return
	}

	// The accountable human is the agent's owner: an agent has no authority of
	// its own, only what the person who created it lent it.
	owner := agent.CreatedBy

	if err := auditBusiness.RecordForPrincipal(
		ctx, &owner, "", auditBusiness.ActorAgent,
		"agent.run", auditBusiness.CategoryAgent,
		auditRunSummary(agent.Name, r.Status, r.ToolsSucceeded, r.DryRun), runAuditMeta(r),
	); err != nil {
		helpers.LogErrorWithContext(ctx,
			"agentRunner: run %s finished but could not be written to the audit log: %v", r.RunID, err)
	}
}

// runAuditMeta is what the audit row carries about a run. Pure, so the
// shape of the record can be asserted without a database.
func runAuditMeta(r finishedRun) map[string]interface{} {
	agent := r.Agent
	toolsSucceeded, failedTools, status := r.ToolsSucceeded, r.FailedTools, r.Status
	meta := map[string]interface{}{
		"agent_id":   agent.Id.String(),
		"agent_name": agent.Name,
		"run_id":     r.RunID.String(),
		"status":     status,
	}
	if t := strings.TrimSpace(r.TriggerSource); t != "" {
		// Whether a person asked for this or it fired on its own is the first
		// thing a reviewer wants and the log did not record it.
		meta["trigger"] = t
	}
	if agent.Remote() {
		// Whose reasoning this was. The host and nothing more: the path may
		// carry a routing token, and the secret never reaches a row.
		meta["remote_brain"] = remoteLabel(agent)
	}
	if r.DryRun {
		// A dry run proposed rather than did. Recording it identically to a run
		// that wrote would overstate what happened, and the difference is the
		// whole point of having a dry run.
		meta["dry_run"] = true
	}
	if len(toolsSucceeded) > 0 {
		meta["tools_succeeded"] = toolsSucceeded
	}
	if len(failedTools) > 0 {
		meta["tools_failed"] = failedTools
	}
	if len(r.GovernanceBlocks) > 0 {
		// The most reviewable fact about a run is what it was refused, and until
		// now the reviewable record was the one place it did not appear.
		meta["governance_blocked"] = r.GovernanceBlocks
	}
	if e := strings.TrimSpace(r.Error); e != "" {
		meta["error"] = helpers.TruncateRunes(e, 512)
	}
	return meta
}

// auditRunSummary is the one line a reviewer reads before deciding whether to
// open the run. It names the agent, what became of the run, and whether
// anything was actually touched, because "finished" and "finished having done
// something" are different events.
//
// Not the runSummary in agentActivity.go, deliberately. That one builds the
// activity feed and prefers the model's own account of what it did, which is
// right for a person catching up and wrong for an audit record: this codebase
// already carries verifyWorkHappened because a run can narrate work no tool
// performed. An audit line is built from what executed.
func auditRunSummary(agentName, status string, toolsSucceeded []string, dryRun bool) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = "an agent"
	}
	switch {
	case dryRun:
		return fmt.Sprintf("%s completed a dry run (%s), proposing rather than writing", name, status)
	case len(toolsSucceeded) == 0:
		return fmt.Sprintf("%s ran (%s) and changed nothing", name, status)
	default:
		return fmt.Sprintf("%s ran (%s) using %s", name, status, strings.Join(toolsSucceeded, ", "))
	}
}
