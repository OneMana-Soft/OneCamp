package Integration

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/postgres/Integration"
	"github.com/google/uuid"
)

func UpsertIntegration(ctx context.Context, entityType string, entityId uuid.UUID, provider string, accessToken *string, refreshToken *string, webhookUrl *string, metadata *string, expiresAt *time.Time) error {
	currentTime := time.Now()
	query := `
		INSERT INTO integrations (entity_type, entity_id, provider, access_token, refresh_token, webhook_url, metadata, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (entity_type, entity_id, provider) DO UPDATE SET
			access_token = EXCLUDED.access_token,
			refresh_token = COALESCE(EXCLUDED.refresh_token, integrations.refresh_token),
			webhook_url = COALESCE(EXCLUDED.webhook_url, integrations.webhook_url),
			metadata = COALESCE(EXCLUDED.metadata, integrations.metadata),
			expires_at = EXCLUDED.expires_at,
			updated_at = EXCLUDED.updated_at
	`
	err := models.UpsertIntegration(query, entityType, entityId, provider, accessToken, refreshToken, webhookUrl, metadata, expiresAt, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpsertIntegration Failed to upsert integration err: %+v", err)
		return err
	}
	return nil
}

func GetIntegration(ctx context.Context, entityType string, entityId uuid.UUID, provider string) (*models.Integration, error) {
	query := `
		SELECT id, entity_type, entity_id, provider, access_token, refresh_token, sync_token, webhook_url, metadata, expires_at, task_sync_enabled, created_at, updated_at
		FROM integrations
		WHERE entity_type = $1 AND entity_id = $2 AND provider = $3
	`
	integration, err := models.GetIntegration(query, entityType, entityId, provider)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetIntegration Failed to get integration err: %+v", err)
		return nil, err
	}
	return integration, nil
}

func UpdateSyncToken(ctx context.Context, entityType string, entityId uuid.UUID, provider, syncToken string) error {
	currentTime := time.Now()
	query := `
		UPDATE integrations
		SET sync_token = $1, updated_at = $2
		WHERE entity_type = $3 AND entity_id = $4 AND provider = $5
	`
	err := models.UpdateSyncToken(query, entityType, entityId, provider, syncToken, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateSyncToken Failed to update sync token err: %+v", err)
		return err
	}
	return nil
}

func DeleteIntegration(ctx context.Context, entityType string, entityId uuid.UUID, provider string) error {
	query := `DELETE FROM integrations WHERE entity_type = $1 AND entity_id = $2 AND provider = $3`
	err := models.DeleteIntegration(query, entityType, entityId, provider)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DeleteIntegration Failed to delete integration err: %+v", err)
		return err
	}
	return nil
}

// GetIntegrationMetadataByGitHubLogin finds GitHub integration metadata by GitHub login.
func GetIntegrationMetadataByGitHubLogin(ctx context.Context, githubLogin string) (*string, error) {
	query := `SELECT metadata FROM integrations WHERE provider = 'github' AND metadata::text LIKE $1 LIMIT 1`
	metadata, err := models.GetIntegrationMetadataByGitHubLogin(query, githubLogin)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetIntegrationMetadataByGitHubLogin Failed err: %+v", err)
		return nil, err
	}
	return metadata, nil
}

// GetIntegrationMetadataByEntityID finds GitHub integration metadata by entity ID.
func GetIntegrationMetadataByEntityID(ctx context.Context, entityId uuid.UUID) (*string, error) {
	query := `SELECT metadata FROM integrations WHERE provider = 'github' AND entity_id = $1 LIMIT 1`
	metadata, err := models.GetIntegrationMetadataByEntityID(query, entityId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetIntegrationMetadataByEntityID Failed err: %+v", err)
		return nil, err
	}
	return metadata, nil
}

func UpdateTaskSyncEnabled(ctx context.Context, entityType string, entityId uuid.UUID, provider string, enabled bool) error {
	currentTime := time.Now()
	query := `
		UPDATE integrations
		SET task_sync_enabled = $1, updated_at = $2
		WHERE entity_type = $3 AND entity_id = $4 AND provider = $5
	`
	return models.UpdateTaskSyncEnabled(query, enabled, currentTime, entityType, entityId, provider)
}

func UpdateIntegrationToken(ctx context.Context, entityType string, entityId uuid.UUID, provider string, accessToken *string, refreshToken *string, expiresAt *time.Time) error {
	currentTime := time.Now()
	query := `
		UPDATE integrations
		SET access_token = $1, refresh_token = $2, expires_at = $3, updated_at = $4
		WHERE entity_type = $5 AND entity_id = $6 AND provider = $7
	`
	return models.UpdateIntegrationToken(query, accessToken, refreshToken, expiresAt, currentTime, entityType, entityId, provider)
}
