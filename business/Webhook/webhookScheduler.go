package business

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// StartWebhookCleanupScheduler starts a background goroutine that cleans up old webhook logs.
// It should be called once during application startup.
func StartWebhookCleanupScheduler() {
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		// Run immediately on startup
		runCleanup()

		for range ticker.C {
			runCleanup()
		}
	}()
}

func runCleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	count, err := CleanupOldLogs(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "webhookCleanup Failed to clean old logs: %+v", err)
		return
	}
	if count > 0 {
		helpers.MessageLogs.InfoLog.Printf("Webhook cleanup: removed %d old log rows", count)
	}
}
