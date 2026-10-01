package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/GitHubWebhookDelivery"
)

// ClaimGitHubWebhookDelivery is the dedup primitive used by the
// inbound webhook handler. It returns true when the caller should run
// the handler for this delivery_id, and false when a previous attempt
// has already marked it 'completed'.
//
// Critically, this differs from RecordGitHubWebhookDelivery in that a
// previously-failed (or still-processing) row CAN be re-claimed by
// GitHub's retry — so transient failures stop being permanent.
func ClaimGitHubWebhookDelivery(ctx context.Context, deliveryID, eventType string) (bool, error) {
	shouldProcess, err := models.ClaimDelivery(ctx, deliveryID, eventType)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ClaimGitHubWebhookDelivery err: %+v", err)
	}
	return shouldProcess, err
}

// MarkGitHubWebhookDeliveryCompleted flips the row to 'completed' so
// a duplicate redelivery is suppressed.
func MarkGitHubWebhookDeliveryCompleted(ctx context.Context, deliveryID string) error {
	return models.MarkDeliveryCompleted(ctx, deliveryID)
}

// MarkGitHubWebhookDeliveryFailed records the latest error so operators
// can debug recurring failures via the database. Does NOT prevent
// GitHub's retry from re-claiming the row on the next delivery.
func MarkGitHubWebhookDeliveryFailed(ctx context.Context, deliveryID, errMsg string) error {
	return models.MarkDeliveryFailed(ctx, deliveryID, errMsg)
}

// GetGitHubWebhookHealth surfaces aggregate inbound delivery stats
// (count of completed / failed / still-processing in the last 24h plus
// the last error message) so admins can spot flaky webhook behaviour
// without trawling the DB.
func GetGitHubWebhookHealth(ctx context.Context) (*models.HealthSummary, error) {
	return models.GetHealthSummary(ctx)
}
