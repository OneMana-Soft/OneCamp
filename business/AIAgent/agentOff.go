package business

// The kill switch, everywhere an agent runs.
//
// ai_agents.is_active is the switch an owner or admin flips to stop an agent,
// and the person an agent works for (created_by) is who it acts as. Every
// credential an agent holds asks the switch per request
// (apiTokenBusiness.BoundAgent), but the runner asked neither: a job queued
// before a pause ran anyway, a run already going carried on to its last step,
// and an agent whose sponsor had left the workspace went on acting as them.
// RunAgent now asks before every step, so however a run was started (a queued
// job, a trigger, a DM, a mention) it stops at the next step; pausing or
// deleting an agent also stops its open jobs, and the trigger workers leave
// out an agent whose sponsor has left.

import (
	"context"
	"database/sql"
	"errors"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// What agentOffReason reads, seams for tests.
var (
	offReadAgent   = model.GetAgentByID
	offReadSponsor = userDomain.GetActiveUserWithAdminFlagByUserUUID
)

// agentOffReason says why an agent may not act right now, or "". It reads the
// agent afresh, not the caller's copy, which a trigger cache can have loaded
// before the pause. A read that fails doesn't stop a run: the switch is a
// state someone set, not a fault, and the step after can read it.
func agentOffReason(ctx context.Context, agentID uuid.UUID) string {
	agent, err := offReadAgent(ctx, agentID)
	switch {
	case err != nil:
		return ""
	case agent == nil:
		return "this AI teammate was deleted"
	case !agent.IsActive:
		return "this AI teammate is paused"
	}
	sponsor, err := offReadSponsor(ctx, agent.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (sponsor == nil || sponsor.Id == uuid.Nil)) {
		return "the person this AI teammate works for has left the workspace"
	}
	return ""
}

// stopAgentWork asks every open job of an agent to stop, as pausing or
// deleting it means: a queued job settles at once, a running one at its next
// heartbeat (the same cooperative stop a person's "Stop" uses).
func stopAgentWork(ctx context.Context, agentID, by uuid.UUID) {
	ids, err := model.RequestAgentTasksCancel(ctx, agentID, by)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AIAgent stopAgentWork (agent=%s) err: %+v", agentID, err)
		return
	}
	for _, id := range ids {
		publishAgentWorkChanged(ctx, id)
	}
}
