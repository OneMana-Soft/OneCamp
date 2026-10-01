package business

// Routine management for the builder UI: list, pause/resume, and cancel an
// agent's routines. Routines are CREATED conversationally (create_routine), but
// an owner/admin manages them here with the same ownership gate as the rest of
// the builder. Read/act only on routines that belong to an agent the actor may
// manage, and only on the agent named in the request (no cross-agent access).

import (
	"context"
	"fmt"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// ListAgentRoutines returns the active routines for an agent the actor may
// manage, newest first. Read-only.
func ListAgentRoutines(ctx context.Context, agentID uuid.UUID, actor Actor) ([]*model.AgentRoutine, error) {
	if _, err := manageableAgent(ctx, agentID, actor); err != nil {
		return nil, err
	}
	return model.ListRoutinesForAgent(ctx, agentID)
}

// SetAgentRoutineEnabled pauses or resumes a routine (without deleting it), for
// an actor who may manage its agent.
func SetAgentRoutineEnabled(ctx context.Context, agentID, routineID uuid.UUID, actor Actor, enabled bool) error {
	if err := ownedRoutine(ctx, agentID, routineID, actor); err != nil {
		return err
	}
	return model.SetRoutineEnabled(ctx, routineID, enabled)
}

// DeleteAgentRoutine cancels (soft-deletes) a routine, for an actor who may
// manage its agent.
func DeleteAgentRoutine(ctx context.Context, agentID, routineID uuid.UUID, actor Actor) error {
	if err := ownedRoutine(ctx, agentID, routineID, actor); err != nil {
		return err
	}
	return model.DeleteRoutine(ctx, routineID)
}

// manageableAgent loads an agent and enforces the builder ownership gate.
func manageableAgent(ctx context.Context, agentID uuid.UUID, actor Actor) (*model.AiAgent, error) {
	a, err := model.GetAgentByID(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent")
	}
	if a == nil {
		return nil, errNotFound
	}
	if !canManage(actor, a) {
		return nil, errForbidden
	}
	return a, nil
}

// ownedRoutine verifies the actor may manage the agent AND the routine belongs
// to that agent (so a routine id from another agent can't be toggled/deleted
// through this agent's endpoint).
func ownedRoutine(ctx context.Context, agentID, routineID uuid.UUID, actor Actor) error {
	if _, err := manageableAgent(ctx, agentID, actor); err != nil {
		return err
	}
	r, err := model.GetRoutine(ctx, routineID)
	if err != nil {
		return fmt.Errorf("failed to load routine")
	}
	if r == nil || r.AgentId != agentID {
		return errNotFound
	}
	return nil
}
