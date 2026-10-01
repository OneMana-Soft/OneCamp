package business

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// StartConfigReconcile keeps this replica's AI service built from the settings
// the store holds now, not the settings it held when this process booted.
//
// SaveConfig and the authorized-models writes call ai.ReloadAIService so the
// replica that took the save applies it at once. That replica is the only one
// they can reach. Every other replica would keep the previous provider, model
// and key until restarted, which for a key that was just rotated is a replica
// that fails every call and for a provider that was just switched is a replica
// still billing the old one. The reconciler's header has the general argument.
//
// Watched: the settings row, the providers it points at, and the authorized
// models, because ResolveConfig reads all three and any of them can change what
// the service is built from.
//
// CALL RIGHT AFTER ai.InitAIService, so the baseline describes the rows the
// service was just built from.
func StartConfigReconcile(ctx context.Context) {
	if err := helpers.StartConfigReconciler(ctx, helpers.ConfigReconciler{
		Name:     "ai-config",
		Interval: helpers.DefaultConfigReconcileInterval,
		Fingerprint: func(ctx context.Context) (string, error) {
			return postgresInit.RowsFingerprint(ctx, "ai_settings", "ai_providers", "ai_authorized_models")
		},
		Apply: ai.ReloadAIService,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "AI config reconciler: %v", err)
	}
}
