package Domain

import (
	"context"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/Webhook"
	"github.com/google/uuid"
)

// SetWebhookScope says what a webhook is set up for: the whole workspace
// ("org", with no entity), a channel or a project.
func SetWebhookScope(ctx context.Context, id uuid.UUID, scopeType string, entity *uuid.UUID, updatedAt time.Time) error {
	query := `UPDATE webhooks SET scope_type = $1, scope_entity_id = $2, updated_at = $3 WHERE id = $4 AND deleted_at IS NULL`
	return logDomainError(ctx, "SetWebhookScope", models.UpdateWebhook(query, scopeType, entity, updatedAt, id))
}

// ChannelIsOpen says whether a channel exists and isn't archived.
func ChannelIsOpen(ctx context.Context, id uuid.UUID) (bool, error) {
	ok, err := models.QueryBool(`SELECT EXISTS (SELECT 1 FROM channels WHERE id = $1 AND deleted_at IS NULL)`, id)
	return ok, logDomainError(ctx, "ChannelIsOpen", err)
}

// ProjectIsOpen says whether a project exists and isn't deleted.
func ProjectIsOpen(ctx context.Context, id uuid.UUID) (bool, error) {
	ok, err := models.QueryBool(`SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)`, id)
	return ok, logDomainError(ctx, "ProjectIsOpen", err)
}

// ChannelIsPublic says whether a channel is a public one. A channel this
// can't find is not.
func ChannelIsPublic(ctx context.Context, id uuid.UUID) (bool, error) {
	ok, err := models.QueryBool(`SELECT EXISTS (SELECT 1 FROM channels WHERE id = $1 AND ch_private = false)`, id)
	return ok, logDomainError(ctx, "ChannelIsPublic", err)
}
