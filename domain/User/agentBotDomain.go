package domain

// Per-agent bot principal (Postgres side). Each Agent Builder agent gets its
// OWN non-login user row, keyed on a deterministic sentinel email derived from
// the agent id, so provisioning is idempotent across restarts and replicas.
// Mirrors the shared automation bot (automationBotDomain.go) but one-per-agent.
//
// Like the shared bot, an agent bot is flagged is_bot + is_external: it can
// never authenticate (no password, excluded from login/SSO) and is excluded
// from human member pickers/rosters the same way ghost/external users are. The
// Dgraph node is provisioned by the business layer (which owns the dual-write).

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// AgentBotEmail returns the stable sentinel email identifying an agent's bot
// principal. Deterministic in the agent id, so EnsureAgentBotUser converges on
// exactly one row per agent. Never change this format — doing so would orphan
// already-provisioned agent bots (and their authored messages).
func AgentBotEmail(agentID uuid.UUID) string {
	return AgentBotUsername(agentID) + BotEmailDomain
}

// AgentBotUsername returns the agent bot's unique @handle-safe username. The
// users.username column is UNIQUE, so it is keyed on the agent id (the display
// name, which CAN collide between agents, is carried separately).
func AgentBotUsername(agentID uuid.UUID) string {
	return AgentBotPrefix + agentID.String()
}

// EnsureAgentBotUser returns the agent's bot principal UUID, creating the
// Postgres row on first call and reconciling its display name on later calls
// (so renaming the agent renames its principal). Idempotent via ON CONFLICT on
// the sentinel email, so concurrent/repeated calls converge on one row.
func EnsureAgentBotUser(ctx context.Context, agentID uuid.UUID, displayName string) (uuid.UUID, error) {
	if displayName == "" {
		displayName = "AI Agent"
	}
	return EnsureBotUser(ctx, AgentBotEmail(agentID), AgentBotUsername(agentID), displayName)
}

// EnsureBotUser returns the bot principal keyed on email, creating its row on
// first call and reconciling its display name on later ones. email must sit
// under BotEmailDomain and username must be unique to it.
func EnsureBotUser(ctx context.Context, email, username, displayName string) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Try to read an existing (non-deleted) agent bot first.
	var id uuid.UUID
	var curDisplayName sql.NullString
	const readQ = `SELECT id, display_name FROM users WHERE email_id = $1 AND deleted_at IS NULL`
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, readQ, email).Scan(&id, &curDisplayName)
	if err == nil {
		// Reconcile the display name if the agent was renamed since seeding.
		// Cheap and only writes on an actual change, so a no-op in steady state.
		if curDisplayName.String != displayName {
			if _, uerr := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
				`UPDATE users SET display_name = $2, updated_at = NOW() WHERE id = $1`,
				id, displayName); uerr != nil {
				helpers.LogErrorWithContext(ctx, "domain/EnsureBotUser reconcile name failed: %+v", uerr)
			}
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "domain/EnsureBotUser read failed: %+v", err)
		return uuid.Nil, err
	}

	// Insert. ON CONFLICT (email_id) handles the race where two instances seed
	// at once; the RETURNING resolves the surviving row's id either way.
	newID := uuid.New()
	const insertQ = `
		INSERT INTO users (id, email_id, username, display_name, is_bot, is_external, created_at, updated_at)
		VALUES ($1, $2, $3, $4, true, true, NOW(), NOW())
		ON CONFLICT (email_id) DO UPDATE SET is_bot = true, is_external = true, display_name = $4, updated_at = NOW()
		RETURNING id`
	if err = postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, insertQ,
		newID, email, username, displayName).Scan(&id); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/EnsureBotUser insert failed: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}
