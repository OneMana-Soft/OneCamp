package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type Webhook struct {
	Id          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	Type        string    `json:"type"`
	// Token is set only on the answer that makes it (CreateWebhook,
	// RegenerateToken): only its hash is kept.
	Token               string     `json:"token,omitempty"`
	Secret              *string    `json:"secret,omitempty"`
	TargetUrl           *string    `json:"target_url,omitempty"`
	ChannelId           *uuid.UUID `json:"channel_id,omitempty"`
	Events              *string    `json:"events,omitempty"`
	IsActive            bool       `json:"is_active"`
	CreatedBy           uuid.UUID  `json:"created_by"`
	BotName             string     `json:"bot_name"`
	BotAvatarUrl        *string    `json:"bot_avatar_url,omitempty"`
	Metadata            *string    `json:"metadata,omitempty"`
	LastTriggeredAt     *time.Time `json:"last_triggered_at,omitempty"`
	FailureCount        int        `json:"failure_count"`
	ScopeType           string     `json:"scope_type"`
	ScopeEntityId       *uuid.UUID `json:"scope_entity_id,omitempty"`
	TargetType          string     `json:"target_type"`
	TriggerWords        *string    `json:"trigger_words,omitempty"`
	ResponseUrl         *string    `json:"response_url,omitempty"`
	SupportsBlocks      bool       `json:"supports_blocks"`
	SupportsEphemeral   bool       `json:"supports_ephemeral"`
	TimeoutMs           int        `json:"timeout_ms"`
	SignatureRequired   bool       `json:"signature_required"`
	Commands            *string    `json:"commands,omitempty"`
	SupportsInteractive bool       `json:"supports_interactive"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	DeletedAt           *time.Time `json:"deleted_at,omitempty"`
	// PausedAt and PausedReason say since when its deliveries wait, and why
	// (business/Hooks: "admin", "failing", "owner_left", "owner_cant_read").
	PausedAt     *time.Time `json:"paused_at,omitempty"`
	PausedReason *string    `json:"paused_reason,omitempty"`
}

func CreateWebhook(query string, id uuid.UUID, name string, description *string, webhookType string, tokenHash []byte, secret *string, targetUrl *string, channelId *uuid.UUID, events *string, createdBy uuid.UUID, botName string, botAvatarUrl *string, metadata *string, createdAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		id,
		name,
		description,
		webhookType,
		tokenHash,
		secret,
		targetUrl,
		channelId,
		events,
		createdBy,
		botName,
		botAvatarUrl,
		metadata,
		createdAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateWebhook Failed to create webhook err: %+v",
			err)
		return err
	}

	return nil
}

// scanner matches both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanFullWebhook reads a full webhook row (32 columns) from s and returns it.
func scanFullWebhook(s scanner) (*Webhook, error) {
	var w Webhook
	var description, secret, targetUrl, events, botAvatarUrl, metadata, scopeType, targetType, triggerWords, responseUrl, commands, pausedReason sql.NullString
	var channelId, scopeEntityId uuid.NullUUID
	var lastTriggeredAt, deletedAt, pausedAt sql.NullTime
	var supportsBlocks, supportsEphemeral, signatureRequired, supportsInteractive sql.NullBool
	var timeoutMs sql.NullInt64

	err := s.Scan(
		&w.Id,
		&w.Name,
		&description,
		&w.Type,
		&secret,
		&targetUrl,
		&channelId,
		&events,
		&w.IsActive,
		&w.CreatedBy,
		&w.BotName,
		&botAvatarUrl,
		&metadata,
		&lastTriggeredAt,
		&w.FailureCount,
		&scopeType,
		&scopeEntityId,
		&targetType,
		&triggerWords,
		&responseUrl,
		&supportsBlocks,
		&supportsEphemeral,
		&timeoutMs,
		&signatureRequired,
		&commands,
		&supportsInteractive,
		&w.CreatedAt,
		&w.UpdatedAt,
		&deletedAt,
		&pausedAt,
		&pausedReason,
	)
	if err != nil {
		return nil, err
	}

	if description.Valid {
		w.Description = &description.String
	}
	if secret.Valid {
		w.Secret = &secret.String
	}
	if targetUrl.Valid {
		w.TargetUrl = &targetUrl.String
	}
	if channelId.Valid {
		w.ChannelId = &channelId.UUID
	}
	if events.Valid {
		w.Events = &events.String
	}
	if botAvatarUrl.Valid {
		w.BotAvatarUrl = &botAvatarUrl.String
	}
	if metadata.Valid {
		w.Metadata = &metadata.String
	}
	if lastTriggeredAt.Valid {
		w.LastTriggeredAt = &lastTriggeredAt.Time
	}
	if deletedAt.Valid {
		w.DeletedAt = &deletedAt.Time
	}
	if scopeType.Valid {
		w.ScopeType = scopeType.String
	}
	if scopeEntityId.Valid {
		w.ScopeEntityId = &scopeEntityId.UUID
	}
	if targetType.Valid {
		w.TargetType = targetType.String
	}
	if triggerWords.Valid {
		w.TriggerWords = &triggerWords.String
	}
	if responseUrl.Valid {
		w.ResponseUrl = &responseUrl.String
	}
	if supportsBlocks.Valid {
		w.SupportsBlocks = supportsBlocks.Bool
	}
	if supportsEphemeral.Valid {
		w.SupportsEphemeral = supportsEphemeral.Bool
	}
	if timeoutMs.Valid {
		w.TimeoutMs = int(timeoutMs.Int64)
	}
	if signatureRequired.Valid {
		w.SignatureRequired = signatureRequired.Bool
	}
	if commands.Valid {
		w.Commands = &commands.String
	}
	if supportsInteractive.Valid {
		w.SupportsInteractive = supportsInteractive.Bool
	}
	if pausedAt.Valid {
		w.PausedAt = &pausedAt.Time
	}
	if pausedReason.Valid {
		w.PausedReason = &pausedReason.String
	}

	return &w, nil
}

func GetWebhookByTokenHash(query string, hash []byte) (*Webhook, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, hash)
	webhook, err := scanFullWebhook(row)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetWebhookByTokenHash Failed to get webhook err: %+v",
			err)
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return webhook, nil
}

func GetWebhookById(query string, id uuid.UUID) (*Webhook, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, id)
	webhook, err := scanFullWebhook(row)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetWebhookById Failed to get webhook err: %+v",
			err)
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return webhook, nil
}

func GetAllWebhooks(query string) ([]*Webhook, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetAllWebhooks Failed to get webhooks err: %+v",
			err)
		return nil, err
	}
	defer rows.Close()

	var webhooks []*Webhook
	for rows.Next() {
		webhook, err := scanFullWebhook(rows)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetAllWebhooks Failed to scan webhook row err: %+v",
				err)
			return nil, err
		}
		webhooks = append(webhooks, webhook)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Webhook rows iteration failed err: %+v", err)
		return nil, err
	}

	return webhooks, nil
}

func UpdateWebhook(query string, args ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateWebhook Failed to update webhook err: %+v",
			err)
		return err
	}

	return nil
}

func SoftDeleteWebhook(query string, deletedAt time.Time, updatedAt time.Time, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, deletedAt, updatedAt, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/SoftDeleteWebhook Failed to soft delete webhook err: %+v",
			err)
		return err
	}

	return nil
}

func GetActiveOutgoingWebhooksByEvent(query string, args ...interface{}) ([]*Webhook, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetActiveOutgoingWebhooksByEvent Failed to get webhooks err: %+v",
			err)
		return nil, err
	}
	defer rows.Close()

	var webhooks []*Webhook
	for rows.Next() {
		var w Webhook
		var secret, targetUrl, metadata, scopeType, triggerWords sql.NullString
		var scopeEntity uuid.NullUUID

		if err := rows.Scan(
			&w.Id,
			&w.Name,
			&secret,
			&targetUrl,
			&metadata,
			&scopeType,
			&triggerWords,
			&scopeEntity,
			// Who set it up: what it may be sent of a private doc or a
			// restricted task is what they may see (business/Webhook dispatch).
			&w.CreatedBy,
		); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetActiveOutgoingWebhooksByEvent Failed to scan row err: %+v",
				err)
			return nil, err
		}

		if secret.Valid {
			w.Secret = &secret.String
		}
		if targetUrl.Valid {
			w.TargetUrl = &targetUrl.String
		}
		if metadata.Valid {
			w.Metadata = &metadata.String
		}
		if scopeType.Valid {
			w.ScopeType = scopeType.String
		} else {
			w.ScopeType = "org"
		}
		if triggerWords.Valid {
			w.TriggerWords = &triggerWords.String
		}
		if scopeEntity.Valid {
			id := scopeEntity.UUID
			w.ScopeEntityId = &id
		}

		webhooks = append(webhooks, &w)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Webhook rows iteration failed err: %+v", err)
		return nil, err
	}

	return webhooks, nil
}

func UpdateWebhookTriggerInfo(query string, lastTriggeredAt time.Time, failureCount int, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, lastTriggeredAt, failureCount, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateWebhookTriggerInfo Failed to update trigger info err: %+v",
			err)
		return err
	}

	return nil
}
