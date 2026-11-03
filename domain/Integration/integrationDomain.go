package Integration

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/Integration"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// An integration disconnected through DeleteIntegration is kept, marked by who
// disconnected it and when, with its credentials cleared (V-184 head 5b, the
// owner's rule that every delete of a business record is soft). Every read and
// write here takes connected (live) rows only, and an integration is unique per
// entity and provider among them, so connecting again makes a new row beside
// the one kept.
func UpsertIntegration(ctx context.Context, entityType string, entityId uuid.UUID, provider string, accessToken *string, refreshToken *string, webhookUrl *string, metadata *string, expiresAt *time.Time) error {
	currentTime := time.Now()
	query := `
		INSERT INTO integrations (entity_type, entity_id, provider, access_token, refresh_token, webhook_url, metadata, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (entity_type, entity_id, provider) WHERE ` + softDelete.Live("") + ` DO UPDATE SET
			access_token = EXCLUDED.access_token,
			refresh_token = COALESCE(EXCLUDED.refresh_token, integrations.refresh_token),
			webhook_url = COALESCE(EXCLUDED.webhook_url, integrations.webhook_url),
			metadata = COALESCE(EXCLUDED.metadata, integrations.metadata),
			expires_at = EXCLUDED.expires_at,
			updated_at = EXCLUDED.updated_at
	`
	_, err := pgexec.Exec(ctx, query, entityType, entityId, provider, accessToken, refreshToken, webhookUrl, metadata, expiresAt, currentTime)
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
		WHERE entity_type = $1 AND entity_id = $2 AND provider = $3 AND ` + softDelete.Live("") + `
	`
	integration, _, err := pgexec.QueryOne(ctx, query, func(r pgexec.Row) (*models.Integration, error) { return models.ScanIntegration(r) }, entityType, entityId, provider)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetIntegration Failed to get integration err: %+v", err)
		return nil, err
	}
	return integration, nil
}

// DeleteIntegration disconnects an integration: its row is kept, marked by
// who disconnected it (by) and when, and its credentials go: both tokens, the
// sync token, the webhook address and the token's expiry. Its metadata stays
// (what was connected, never a secret). Disconnecting what isn't connected
// changes nothing.
func DeleteIntegration(ctx context.Context, entityType string, entityId uuid.UUID, provider string, by uuid.UUID) error {
	query := `UPDATE integrations
		SET deleted_at = NOW(), deleted_by = $4, access_token = NULL, refresh_token = NULL,
		    sync_token = NULL, webhook_url = NULL, expires_at = NULL, updated_at = NOW()
		WHERE entity_type = $1 AND entity_id = $2 AND provider = $3 AND ` + softDelete.Live("")
	_, err := pgexec.Exec(ctx, query, entityType, entityId, provider, softDelete.By(by))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DeleteIntegration Failed to disconnect integration err: %+v", err)
		return err
	}
	return nil
}

// GetIntegrationMetadataByGitHubLogin finds GitHub integration metadata by GitHub login.
func GetIntegrationMetadataByGitHubLogin(ctx context.Context, githubLogin string) (*string, error) {
	query := `SELECT metadata FROM integrations WHERE provider = 'github' AND metadata::text LIKE $1 AND ` + softDelete.Live("") + ` LIMIT 1`
	metadata, _, err := pgexec.QueryOne(ctx, query, scanMetadata, "%"+githubLogin+"%")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetIntegrationMetadataByGitHubLogin Failed err: %+v", err)
		return nil, err
	}
	return metadata, nil
}

// GetIntegrationMetadataByEntityID finds GitHub integration metadata by entity ID.
func GetIntegrationMetadataByEntityID(ctx context.Context, entityId uuid.UUID) (*string, error) {
	query := `SELECT metadata FROM integrations WHERE provider = 'github' AND entity_id = $1 AND ` + softDelete.Live("") + ` LIMIT 1`
	metadata, _, err := pgexec.QueryOne(ctx, query, scanMetadata, entityId)
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
		WHERE entity_type = $3 AND entity_id = $4 AND provider = $5 AND ` + softDelete.Live("") + `
	`
	_, err := pgexec.Exec(ctx, query, enabled, currentTime, entityType, entityId, provider)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateTaskSyncEnabled err: %+v", err)
	}
	return err
}

func UpdateIntegrationToken(ctx context.Context, entityType string, entityId uuid.UUID, provider string, accessToken *string, refreshToken *string, expiresAt *time.Time) error {
	currentTime := time.Now()
	query := `
		UPDATE integrations
		SET access_token = $1, refresh_token = $2, expires_at = $3, updated_at = $4
		WHERE entity_type = $5 AND entity_id = $6 AND provider = $7 AND ` + softDelete.Live("") + `
	`
	_, err := pgexec.Exec(ctx, query, accessToken, refreshToken, expiresAt, currentTime, entityType, entityId, provider)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateIntegrationToken err: %+v", err)
	}
	return err
}

// scanMetadata reads an integration's metadata (nil for none).
func scanMetadata(r pgexec.Row) (m *string, err error) { err = r.Scan(&m); return }
