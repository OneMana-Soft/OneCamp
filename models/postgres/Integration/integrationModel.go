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

// Provider names — keep in sync with anywhere a string literal is
// passed to UpsertIntegration / GetIntegration / DeleteIntegration.
const (
	ProviderGoogleCalendar = "google_calendar"
	ProviderGitHub         = "github"
	ProviderSlackBot       = "slack_bot"
)

// EntityType values used by the integrations table.
const (
	IntegrationEntityOrg     = "org"
	IntegrationEntityUser    = "user"
	IntegrationEntityProject = "project"
	IntegrationEntityTeam    = "team"
	IntegrationEntityChannel = "channel"
)

type Integration struct {
	Id              uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	EntityType      string    // see IntegrationEntity* constants
	EntityId        uuid.UUID
	Provider        string // see Provider* constants
	AccessToken     *string
	RefreshToken    *string
	SyncToken       *string
	WebhookUrl      *string
	Metadata        *string // jsonb stored as string
	ExpiresAt       *time.Time
	TaskSyncEnabled bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func UpsertIntegration(query string, entityType string, entityId uuid.UUID, provider string, accessToken *string, refreshToken *string, webhookUrl *string, metadata *string, expiresAt *time.Time, updatedAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		entityType,
		entityId,
		provider,
		accessToken,
		refreshToken,
		webhookUrl,
		metadata,
		expiresAt,
		updatedAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpsertIntegration Failed to upsert integration err: %+v", err)
		return err
	}
	return nil
}

func GetIntegration(query string, entityType string, entityId uuid.UUID, provider string) (*Integration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var integration Integration
	var accessToken sql.NullString
	var refreshToken sql.NullString
	var syncToken sql.NullString
	var webhookUrl sql.NullString
	var metadata sql.NullString
	var expiresAt sql.NullTime
	var taskSyncEnabled bool

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, entityType, entityId, provider)
	err := row.Scan(
		&integration.Id,
		&integration.EntityType,
		&integration.EntityId,
		&integration.Provider,
		&accessToken,
		&refreshToken,
		&syncToken,
		&webhookUrl,
		&metadata,
		&expiresAt,
		&taskSyncEnabled,
		&integration.CreatedAt,
		&integration.UpdatedAt,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "models/GetIntegration Failed to get integration err: %+v", err)
		return nil, err
	}

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // No integration found for this provider
	}

	if accessToken.Valid {
		integration.AccessToken = &accessToken.String
	}
	if refreshToken.Valid {
		integration.RefreshToken = &refreshToken.String
	}
	if syncToken.Valid {
		integration.SyncToken = &syncToken.String
	}
	if webhookUrl.Valid {
		integration.WebhookUrl = &webhookUrl.String
	}
	if metadata.Valid {
		integration.Metadata = &metadata.String
	}
	if expiresAt.Valid {
		integration.ExpiresAt = &expiresAt.Time
	}
	integration.TaskSyncEnabled = taskSyncEnabled

	return &integration, nil
}

func UpdateSyncToken(query string, entityType string, entityId uuid.UUID, provider string, syncToken string, updatedAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		syncToken,
		updatedAt,
		entityType,
		entityId,
		provider,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateSyncToken Failed to update sync token err: %+v", err)
		return err
	}
	return nil
}

func DeleteIntegration(query string, entityType string, entityId uuid.UUID, provider string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		entityType,
		entityId,
		provider,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteIntegration Failed to delete integration err: %+v", err)
		return err
	}
	return nil
}

func UpdateTaskSyncEnabled(query string, enabled bool, updatedAt time.Time, entityType string, entityId uuid.UUID, provider string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		enabled,
		updatedAt,
		entityType,
		entityId,
		provider,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateTaskSyncEnabled Failed to update task sync enabled err: %+v", err)
		return err
	}
	return nil
}

func UpdateIntegrationToken(query string, accessToken *string, refreshToken *string, expiresAt *time.Time, updatedAt time.Time, entityType string, entityId uuid.UUID, provider string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		accessToken,
		refreshToken,
		expiresAt,
		updatedAt,
		entityType,
		entityId,
		provider,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateIntegrationToken Failed to update integration token err: %+v", err)
		return err
	}
	return nil
}

// GetIntegrationMetadataByGitHubLogin finds integration metadata matching a GitHub login.
func GetIntegrationMetadataByGitHubLogin(query string, githubLogin string) (*string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var metadataStr *string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, "%"+githubLogin+"%").Scan(&metadataStr)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return metadataStr, nil
}

// GetIntegrationMetadataByEntityID finds integration metadata by entity ID.
func GetIntegrationMetadataByEntityID(query string, entityId uuid.UUID) (*string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var metadataStr *string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, entityId).Scan(&metadataStr)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return metadataStr, nil
}
