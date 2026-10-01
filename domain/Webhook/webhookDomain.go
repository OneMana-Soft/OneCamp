package Domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/Webhook"
	logModels "github.com/akashc777/OneCamp/models/postgres/WebhookLog"
	"github.com/google/uuid"
)

const WEBHOOK_SELECT_ALL_COLS = `id, name, description, type, token, secret, target_url, channel_id, events, is_active, created_by, bot_name, bot_avatar_url, metadata, last_triggered_at, failure_count, scope_type, scope_entity_id, target_type, trigger_words, response_url, supports_blocks, supports_ephemeral, timeout_ms, signature_required, commands, supports_interactive, created_at, updated_at, deleted_at`

func logDomainError(ctx context.Context, op string, err error) error {
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/%s Failed err: %+v", op, err)
	}
	return err
}

func CreateWebhook(ctx context.Context, id uuid.UUID, name string, description *string, webhookType string, token string, secret *string, targetUrl *string, channelId *uuid.UUID, events *string, createdBy uuid.UUID, botName string, botAvatarUrl *string, metadata *string, createdAt time.Time) error {
	query := `
		INSERT INTO webhooks (id, name, description, type, token, secret, target_url, channel_id, events, created_by, bot_name, bot_avatar_url, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
	`
	return logDomainError(ctx, "CreateWebhook", models.CreateWebhook(query, id, name, description, webhookType, token, secret, targetUrl, channelId, events, createdBy, botName, botAvatarUrl, metadata, createdAt))
}

func GetWebhookByToken(ctx context.Context, token string) (*models.Webhook, error) {
	query := `SELECT ` + WEBHOOK_SELECT_ALL_COLS + ` FROM webhooks WHERE token = $1 AND deleted_at IS NULL`
	webhook, err := models.GetWebhookByToken(query, token)
	return webhook, logDomainError(ctx, "GetWebhookByToken", err)
}

func GetWebhookById(ctx context.Context, id uuid.UUID) (*models.Webhook, error) {
	query := `SELECT ` + WEBHOOK_SELECT_ALL_COLS + ` FROM webhooks WHERE id = $1 AND deleted_at IS NULL`
	webhook, err := models.GetWebhookById(query, id)
	return webhook, logDomainError(ctx, "GetWebhookById", err)
}

func GetAllWebhooks(ctx context.Context) ([]*models.Webhook, error) {
	query := `SELECT ` + WEBHOOK_SELECT_ALL_COLS + ` FROM webhooks WHERE deleted_at IS NULL ORDER BY created_at DESC`
	webhooks, err := models.GetAllWebhooks(query)
	return webhooks, logDomainError(ctx, "GetAllWebhooks", err)
}

func UpdateWebhook(ctx context.Context, id uuid.UUID, name string, description *string, targetUrl *string, channelId *uuid.UUID, events *string, isActive bool, botName string, botAvatarUrl *string, metadata *string, updatedAt time.Time) error {
	query := `
		UPDATE webhooks
		SET name = $1, description = $2, target_url = $3, channel_id = $4, events = $5, is_active = $6, bot_name = $7, bot_avatar_url = $8, metadata = $9, updated_at = $10
		WHERE id = $11 AND deleted_at IS NULL
	`
	return logDomainError(ctx, "UpdateWebhook", models.UpdateWebhook(query, name, description, targetUrl, channelId, events, isActive, botName, botAvatarUrl, metadata, updatedAt, id))
}

func RegenerateWebhookToken(ctx context.Context, id uuid.UUID, newToken string, updatedAt time.Time) error {
	query := `UPDATE webhooks SET token = $1, updated_at = $2 WHERE id = $3 AND deleted_at IS NULL`
	return logDomainError(ctx, "RegenerateWebhookToken", models.UpdateWebhook(query, newToken, updatedAt, id))
}

func RegenerateWebhookSecret(ctx context.Context, id uuid.UUID, newSecret string, updatedAt time.Time) error {
	query := `UPDATE webhooks SET secret = $1, updated_at = $2 WHERE id = $3 AND deleted_at IS NULL`
	return logDomainError(ctx, "RegenerateWebhookSecret", models.UpdateWebhook(query, newSecret, updatedAt, id))
}

func SoftDeleteWebhook(ctx context.Context, id uuid.UUID, deletedAt time.Time) error {
	query := `UPDATE webhooks SET deleted_at = $1, updated_at = $2 WHERE id = $3 AND deleted_at IS NULL`
	return logDomainError(ctx, "SoftDeleteWebhook", models.SoftDeleteWebhook(query, deletedAt, deletedAt, id))
}

func GetActiveOutgoingWebhooksByEvent(ctx context.Context, eventType string, scopeType string, scopeEntityId *uuid.UUID) ([]*models.Webhook, error) {
	query := `
		SELECT id, name, token, secret, target_url, metadata, scope_type, trigger_words
		FROM webhooks
		WHERE type = 'outgoing'
		  AND is_active = true
		  AND deleted_at IS NULL
		  AND failure_count < 10
		  AND events @> $1::jsonb
	`
	var args []interface{}
	args = append(args, `"`+eventType+`"`)
	if scopeType != "" && scopeType != "org" && scopeEntityId != nil {
		query += ` AND scope_type = $2 AND scope_entity_id = $3`
		args = append(args, scopeType, scopeEntityId)
	} else {
		query += ` AND scope_type = 'org'`
	}

	webhooks, err := models.GetActiveOutgoingWebhooksByEvent(query, args...)
	return webhooks, logDomainError(ctx, "GetActiveOutgoingWebhooksByEvent", err)
}

func UpdateWebhookSignatureRequired(ctx context.Context, id uuid.UUID, signatureRequired bool, updatedAt time.Time) error {
	query := `UPDATE webhooks SET signature_required = $1, updated_at = $2 WHERE id = $3 AND deleted_at IS NULL`
	return logDomainError(ctx, "UpdateWebhookSignatureRequired", models.UpdateWebhook(query, signatureRequired, updatedAt, id))
}

func UpdateWebhookTriggerInfo(ctx context.Context, id uuid.UUID, lastTriggeredAt time.Time, failureCount int) error {
	query := `UPDATE webhooks SET last_triggered_at = $1, failure_count = $2 WHERE id = $3`
	return logDomainError(ctx, "UpdateWebhookTriggerInfo", models.UpdateWebhookTriggerInfo(query, lastTriggeredAt, failureCount, id))
}

// IncrementWebhookFailureCount atomically increments failure_count and returns the new failure_count value.
func IncrementWebhookFailureCount(ctx context.Context, id uuid.UUID, lastTriggeredAt time.Time) (int, error) {
	query := `UPDATE webhooks SET failure_count = failure_count + 1, last_triggered_at = $1 WHERE id = $2 RETURNING failure_count`
	newCount, err := models.IncrementWebhookFailureCount(query, lastTriggeredAt, id)
	return newCount, logDomainError(ctx, "IncrementWebhookFailureCount", err)
}

func DisableWebhook(ctx context.Context, id uuid.UUID, updatedAt time.Time) error {
	query := `UPDATE webhooks SET is_active = false, updated_at = $1 WHERE id = $2`
	return logDomainError(ctx, "DisableWebhook", models.UpdateWebhook(query, updatedAt, id))
}

// Webhook Logs

func CreateWebhookLog(ctx context.Context, webhookId uuid.UUID, eventType string, requestBody *string, responseStatus *int, responseBody *string, errorMessage *string, durationMs *int, success bool) error {
	query := `
		INSERT INTO webhook_logs (id, webhook_id, event_type, request_body, response_status, response_body, error_message, duration_ms, success, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	id := uuid.New()
	createdAt := time.Now()
	return logDomainError(ctx, "CreateWebhookLog", logModels.CreateWebhookLog(query, id, webhookId, eventType, requestBody, responseStatus, responseBody, errorMessage, durationMs, success, createdAt))
}

func GetWebhookLogs(ctx context.Context, webhookId uuid.UUID, limit int, offset int) ([]*logModels.WebhookLog, error) {
	query := `
		SELECT id, webhook_id, event_type, request_body, response_status, response_body, error_message, duration_ms, success, created_at
		FROM webhook_logs
		WHERE webhook_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`
	logs, err := logModels.GetWebhookLogsByWebhookId(query, webhookId, limit, offset)
	return logs, logDomainError(ctx, "GetWebhookLogs", err)
}

func GetWebhookLogsGlobal(ctx context.Context, webhookId *uuid.UUID, success *bool, fromDate *time.Time, toDate *time.Time, limit int, offset int) ([]*logModels.WebhookLog, error) {
	query := `
		SELECT id, webhook_id, event_type, request_body, response_status, response_body, error_message, duration_ms, success, created_at
		FROM webhook_logs
		WHERE ($1::uuid IS NULL OR webhook_id = $1)
		  AND ($2::boolean IS NULL OR success = $2)
		  AND ($3::timestamp IS NULL OR created_at >= $3)
		  AND ($4::timestamp IS NULL OR created_at <= $4)
		ORDER BY created_at DESC
		LIMIT $5 OFFSET $6
	`
	logs, err := logModels.GetWebhookLogsGlobal(query, webhookId, success, fromDate, toDate, limit, offset)
	return logs, logDomainError(ctx, "GetWebhookLogsGlobal", err)
}

func CleanupOldWebhookLogs(ctx context.Context, retentionDays int) (int64, error) {
	query := `DELETE FROM webhook_logs WHERE created_at < $1`
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	count, err := logModels.DeleteOldWebhookLogs(query, cutoff)
	return count, logDomainError(ctx, "CleanupOldWebhookLogs", err)
}
