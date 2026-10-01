package domain

// Automation bot identity — the shared, non-login "OneCamp AI" user that
// webhooks, workflows, and the AI coworker post through. Persisted like an
// external/ghost user (a real Postgres row + Dgraph node) but flagged is_bot and pinned to a
// stable sentinel email so seeding is idempotent across restarts.

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// SystemBotEmail is the stable sentinel identifying the single workspace-level
// automation bot. Matches the partial unique index in migration 77.
//
// IMPORTANT: this is the immutable key the bot row is keyed on. Never change
// it — doing so would orphan the existing bot user (and all its authored
// messages) in already-deployed workspaces. The bot's DISPLAY identity
// (username/display name) can change freely and is reconciled on startup.
const SystemBotEmail = "automation@bot.onecamp.local"

// The bot's user-facing identity (the @mention handle and the name shown on its
// messages) depends on the edition, because the same principal does two
// different jobs. Per-webhook or per-workflow display labels are layered on top
// at post time.
//
// On the AI edition it is the workspace assistant: it posts recaps, answers
// @mentions and runs agents, so "OneCamp AI" describes it.
//
// On the AI-free edition none of that exists. The principal is still there,
// because workflows post through it, but it does nothing whatsoever with AI. It
// was still called "OneCamp AI", so a customer who deliberately bought the
// edition without AI got a bot named after the feature they had opted out of,
// posting their workflow messages. No frontend guard could catch that: the
// string is seeded into the database by the backend.
//
// Keyed on FeatureRegistered rather than FeatureStatus so the name follows the
// BUILD and not the current configuration. See the comment there.
//
// Safe to change: EnsureSystemBotUser reconciles the Postgres row and
// EnsureAutomationBot re-upserts the Dgraph node on every startup, so a rename
// here propagates to already-seeded deployments on next boot.
func SystemBotUsername() string {
	if helpers.FeatureRegistered(helpers.FeatureNameAI) {
		return "onecamp-ai"
	}
	return "onecamp-automations"
}

// SystemBotDisplayName is the name shown on everything the bot posts.
func SystemBotDisplayName() string {
	if helpers.FeatureRegistered(helpers.FeatureNameAI) {
		return "OneCamp AI"
	}
	return "OneCamp Automations"
}

// EnsureSystemBotUser returns the system automation bot's UUID, creating the
// Postgres row on first call. Idempotent via ON CONFLICT on the sentinel email,
// so concurrent/repeated startup calls converge on one row. The Dgraph node is
// provisioned by the business layer (which owns the dual-write).
func EnsureSystemBotUser(ctx context.Context) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Try to read an existing (non-deleted) bot first.
	var id uuid.UUID
	var curUsername, curDisplayName sql.NullString
	readQ := `SELECT id, username, display_name FROM users WHERE email_id = $1 AND deleted_at IS NULL`
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, readQ, SystemBotEmail).Scan(&id, &curUsername, &curDisplayName)
	if err == nil {
		// Reconcile the display identity in case the canonical name changed
		// since this workspace was seeded (e.g. a rename of the bot). Cheap,
		// runs once at startup, and only writes when something actually
		// differs so it stays a no-op on the steady state.
		if curUsername.String != SystemBotUsername() || curDisplayName.String != SystemBotDisplayName() {
			if _, uerr := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
				`UPDATE users SET username = $2, display_name = $3, updated_at = NOW() WHERE id = $1`,
				id, SystemBotUsername(), SystemBotDisplayName()); uerr != nil {
				// Non-fatal: the bot still works under its old name; log and continue.
				helpers.LogErrorWithContext(ctx, "domain/EnsureSystemBotUser reconcile name failed: %+v", uerr)
			}
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "domain/EnsureSystemBotUser read failed: %+v", err)
		return uuid.Nil, err
	}

	// Insert. ON CONFLICT (email) handles the race where two instances seed at
	// once; the RETURNING/SELECT below resolves the surviving row's id either
	// way. username/display_name carry the bot's identity.
	newID := uuid.New()
	insertQ := `
		INSERT INTO users (id, email_id, username, display_name, is_bot, is_external, created_at, updated_at)
		VALUES ($1, $2, $3, $4, true, true, NOW(), NOW())
		ON CONFLICT (email_id) DO UPDATE SET is_bot = true, is_external = true, updated_at = NOW()
		RETURNING id`
	err = postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, insertQ,
		newID, SystemBotEmail, SystemBotUsername(), SystemBotDisplayName()).Scan(&id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/EnsureSystemBotUser insert failed: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}
