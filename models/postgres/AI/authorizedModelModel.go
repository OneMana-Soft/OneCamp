package models

// Data access for the admin-authorized model allowlist and per-user model
// choice (migration 81).
//
// The workspace still has ONE default chat model in ai_settings; this layer
// adds an admin-managed SET of models a member may pick from for their own
// assistant. The default is always usable and is the fallback when a user has
// no pick or their pick was revoked.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

var ErrAuthorizedModelNotFound = errors.New("authorized model not found")

// AuthorizedModel is one entry in the admin allowlist, joined with its
// provider so callers can build an endpoint without a second lookup.
type AuthorizedModel struct {
	ID           uuid.UUID `json:"id"`
	ProviderID   uuid.UUID `json:"provider_id"`
	ProviderKind string    `json:"provider_kind"`
	ProviderName string    `json:"provider_label"`
	Model        string    `json:"model"`
	Label        string    `json:"label"`
	Enabled      bool      `json:"enabled"`
	// ProviderEnabled mirrors ai_providers.enabled. A model whose provider is
	// disabled is not actually usable even if its own enabled flag is true.
	ProviderEnabled bool `json:"provider_enabled"`
	// ContextWindowTokens / MaxOutputTokens are this model's own token limits
	// (migration 140). 0 means "inherit the workspace value" and is the default, so a
	// workspace that has never touched them behaves exactly as before.
	//
	// They live here, on the row where an admin names the model, because a context
	// window is a property of the MODEL and not of the workspace. The single
	// ai_settings window every budget used to read cannot be right for more than one
	// model at a time, and this product lets a member, a channel and an agent each
	// pick a different one.
	ContextWindowTokens int       `json:"context_window_tokens"`
	MaxOutputTokens     int       `json:"max_output_tokens"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// DisplayLabel returns the friendly label, falling back to the model tag.
func (m *AuthorizedModel) DisplayLabel() string {
	if m.Label != "" {
		return m.Label
	}
	return m.Model
}

// Usable reports whether this model can actually serve traffic: both it and
// its provider must be enabled.
func (m *AuthorizedModel) Usable() bool {
	return m != nil && m.Enabled && m.ProviderEnabled
}

const authorizedModelSelect = `
	SELECT m.id, m.provider_id, p.kind, p.label, m.model, m.label,
	       m.enabled, p.enabled, m.context_window_tokens, m.max_output_tokens,
	       m.updated_at
	FROM ai_authorized_models m
	JOIN ai_providers p ON p.id = m.provider_id`

func scanAuthorizedModel(s scannable) (*AuthorizedModel, error) {
	var m AuthorizedModel
	var updatedAt sql.NullTime
	if err := s.Scan(&m.ID, &m.ProviderID, &m.ProviderKind, &m.ProviderName,
		&m.Model, &m.Label, &m.Enabled, &m.ProviderEnabled,
		&m.ContextWindowTokens, &m.MaxOutputTokens, &updatedAt); err != nil {
		return nil, err
	}
	if updatedAt.Valid {
		m.UpdatedAt = updatedAt.Time
	}
	return &m, nil
}

// ListAuthorizedModels returns every entry in the allowlist, provider-grouped
// then by label. Used by the admin UI (shows disabled rows too).
func ListAuthorizedModels(ctx context.Context) ([]*AuthorizedModel, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := authorizedModelSelect + `
		ORDER BY p.is_builtin DESC, lower(p.label) ASC, lower(m.model) ASC`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q)
	if err != nil {
		return nil, fmt.Errorf("list authorized models: %w", err)
	}
	defer rows.Close()

	var out []*AuthorizedModel
	for rows.Next() {
		m, err := scanAuthorizedModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Limit bounds shared by the SQL CHECK constraints in migration 140 and the Go guard in
// SetAuthorizedModelLimits. The database is the real enforcement — a value outside these
// is rejected however it arrives — and Go restates them only to fail with a readable
// message instead of a driver error.
const (
	MinModelContextWindow = 2048
	MaxModelContextWindow = 20_000_000
	MinModelMaxOutput     = 256
	MaxModelMaxOutput     = 1_000_000
)

// SetAuthorizedModelLimits records what an admin knows about one model's token limits.
// 0 for either value clears it back to "inherit the workspace window", which is also the
// default — so this is always reversible to the prior behaviour by writing zeros rather
// than by deleting the row.
//
// Deliberately NOT folded into CreateAuthorizedModel's upsert: re-authorizing a model is
// idempotent and refreshes its label, and must not silently reset limits an admin set
// earlier. Naming a model again is not a statement about its context window.
func SetAuthorizedModelLimits(ctx context.Context, id uuid.UUID, contextWindowTokens, maxOutputTokens int) error {
	if contextWindowTokens < 0 || maxOutputTokens < 0 {
		return fmt.Errorf("model limits cannot be negative")
	}
	if contextWindowTokens != 0 && (contextWindowTokens < MinModelContextWindow || contextWindowTokens > MaxModelContextWindow) {
		return fmt.Errorf("context window must be 0 (inherit) or between %d and %d tokens, got %d",
			MinModelContextWindow, MaxModelContextWindow, contextWindowTokens)
	}
	if maxOutputTokens != 0 && (maxOutputTokens < MinModelMaxOutput || maxOutputTokens > MaxModelMaxOutput) {
		return fmt.Errorf("max output must be 0 (inherit) or between %d and %d tokens, got %d",
			MinModelMaxOutput, MaxModelMaxOutput, maxOutputTokens)
	}

	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_authorized_models
		    SET context_window_tokens = $1, max_output_tokens = $2, updated_at = NOW()
		  WHERE id = $3`, contextWindowTokens, maxOutputTokens, id)
	if err != nil {
		return fmt.Errorf("set authorized model limits: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrAuthorizedModelNotFound
	}
	return nil
}

// GetAuthorizedModelByProviderModel returns the allowlist entry for a (provider, model)
// pair, or (nil, nil) when that pair is not on the allowlist.
//
// Exists because the model-resolution path identifies a model by provider+tag rather
// than by allowlist id — an agent and a channel both store the pair — and it needs the
// row to learn that model's limits. A miss is not an error: the workspace default model
// is resolvable without being on the allowlist, and the caller falls back to the
// workspace window.
func GetAuthorizedModelByProviderModel(ctx context.Context, providerID uuid.UUID, model string) (*AuthorizedModel, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		authorizedModelSelect+` WHERE m.provider_id = $1 AND m.model = $2`, providerID, model)
	m, err := scanAuthorizedModel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get authorized model by provider/model: %w", err)
	}
	return m, nil
}

// GetAuthorizedModel returns a single allowlist entry by id.
func GetAuthorizedModel(ctx context.Context, id uuid.UUID) (*AuthorizedModel, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, authorizedModelSelect+` WHERE m.id = $1`, id)
	m, err := scanAuthorizedModel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAuthorizedModelNotFound
	}
	return m, err
}

// CreateAuthorizedModel adds a model to the allowlist. The provider must
// exist (FK). Re-authorizing an existing (provider, model) is idempotent: it
// re-enables it and refreshes the label rather than erroring.
func CreateAuthorizedModel(ctx context.Context, providerID uuid.UUID, model, label string) (*AuthorizedModel, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		INSERT INTO ai_authorized_models (provider_id, model, label, enabled)
		VALUES ($1, $2, $3, true)
		ON CONFLICT (provider_id, model)
		DO UPDATE SET label = EXCLUDED.label, enabled = true, updated_at = NOW()
		RETURNING id`

	var id uuid.UUID
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, providerID, model, label).Scan(&id); err != nil {
		return nil, fmt.Errorf("create authorized model: %w", err)
	}
	return GetAuthorizedModel(ctx, id)
}

// SetAuthorizedModelEnabled toggles a single allowlist entry without deleting
// it (and without disturbing user preferences pointing at it).
func SetAuthorizedModelEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_authorized_models SET enabled = $1, updated_at = NOW() WHERE id = $2`, enabled, id)
	if err != nil {
		return fmt.Errorf("set authorized model enabled: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrAuthorizedModelNotFound
	}
	return nil
}

// DeleteAuthorizedModel removes a model from the allowlist. Any user
// preference pointing at it is reset to NULL by the FK (ON DELETE SET NULL),
// so those members silently fall back to the workspace default.
func DeleteAuthorizedModel(ctx context.Context, id uuid.UUID) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`DELETE FROM ai_authorized_models WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete authorized model: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrAuthorizedModelNotFound
	}
	return nil
}

// ─── Per-user preference ──────────────────────────────────────────────

// GetUserModelPreference returns the user's chosen model joined with its
// provider, or (nil, nil) when the user has no pick, the pick was revoked,
// or the model/provider is disabled. Callers treat nil as "use the workspace
// default", so a revoked or disabled model degrades gracefully.
func GetUserModelPreference(ctx context.Context, userUUID string) (*AuthorizedModel, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := authorizedModelSelect + `
		JOIN user_ai_model_preference up ON up.authorized_model_id = m.id
		WHERE up.user_uuid = $1`

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, userUUID)
	m, err := scanAuthorizedModel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user model preference: %w", err)
	}
	if !m.Usable() {
		return nil, nil
	}
	return m, nil
}

// SetUserModelPreference upserts a user's chosen model. A nil id clears the
// preference (revert to the workspace default). A non-nil id is validated to
// be an enabled, usable allowlist entry so a member can never bind to a model
// the admin has not authorized.
func SetUserModelPreference(ctx context.Context, userUUID string, modelID *uuid.UUID) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if modelID != nil {
		m, err := GetAuthorizedModel(ctx, *modelID)
		if err != nil {
			return err
		}
		if !m.Usable() {
			return fmt.Errorf("model %q is not available", m.DisplayLabel())
		}
	}

	const q = `
		INSERT INTO user_ai_model_preference (user_uuid, authorized_model_id, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_uuid)
		DO UPDATE SET authorized_model_id = EXCLUDED.authorized_model_id, updated_at = NOW()`

	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, userUUID, modelID); err != nil {
		return fmt.Errorf("set user model preference: %w", err)
	}
	return nil
}

// GetUserCustomInstructions returns the member's personal AI custom
// instructions, or "" when they have none (no row, or an empty value). Read on
// the assistant hot path, so it is a single indexed point lookup.
func GetUserCustomInstructions(ctx context.Context, userUUID string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var instructions string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT custom_instructions FROM user_ai_model_preference WHERE user_uuid = $1`, userUUID).Scan(&instructions)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get user custom instructions: %w", err)
	}
	return instructions, nil
}

// SetUserCustomInstructions upserts the member's personal AI custom
// instructions independently of any model pick (the row may exist with a null
// model). An empty string clears them (restores default assistant behavior).
func SetUserCustomInstructions(ctx context.Context, userUUID, instructions string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		INSERT INTO user_ai_model_preference (user_uuid, custom_instructions, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_uuid)
		DO UPDATE SET custom_instructions = EXCLUDED.custom_instructions, updated_at = NOW()`

	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, userUUID, instructions); err != nil {
		return fmt.Errorf("set user custom instructions: %w", err)
	}
	return nil
}
