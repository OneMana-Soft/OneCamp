package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	emailLogModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailLog"
	emailQueueModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailQueue"
	suppressionModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailSuppression"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

const (
	// pollInterval is the floor for how often the worker wakes up when
	// nothing has been signalled. It also runs on every EmailWorkerSignal,
	// so under load (e.g. mention storm) it processes the queue near-
	// instantly.
	pollInterval = 15 * time.Second

	// staleAfter is how long a 'processing' row may sit before being
	// reaped back to 'pending'. Picked to comfortably exceed the worker
	// timeout for a single Resend call.
	staleAfter = 5 * time.Minute

	// retentionTerminalRows controls how long we keep terminal-state queue
	// rows. The log table retains a longer-lived audit trail.
	retentionTerminalRows = 7 * 24 * time.Hour

	// retentionLogRows is the audit-trail TTL. 90 days is the default; an
	// admin setting could override later.
	retentionLogRows = 90 * 24 * time.Hour

	// maxAttempts is the cap on retry attempts for transient failures.
	maxAttempts = 5

	// batchSize is the maximum number of rows the worker claims per cycle.
	// Bounded so a giant backlog doesn't starve the rest of the system.
	batchSize = 25

	// concurrencyDefault is the parallelism within a batch. Override via
	// EMAIL_WORKER_CONCURRENCY.
	concurrencyDefault = 5
)

// StartEmailWorker runs the email queue worker in a background goroutine.
// It is a no-op if the email feature flag is off (RESEND_API_KEY unset).
//
// Lifecycle: respects the supplied shutdownCtx for graceful exit. The same
// ctx is used by every Resend HTTP call so server shutdown cancels in-flight
// network requests promptly.
func StartEmailWorker(shutdownCtx context.Context) {
	if !emailService.NotificationEmailEnabled() {
		helpers.MessageLogs.InfoLog.Println("Email worker not started (no sending key, or essentials only)")
		return
	}

	go workerLoop(shutdownCtx)
	go cleanupLoop(shutdownCtx)
	helpers.MessageLogs.InfoLog.Println("Email worker started")
}

func workerLoop(shutdownCtx context.Context) {
	// Brief startup pause so other initializers settle (consistent with
	// the GitHub sync worker).
	time.Sleep(8 * time.Second)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					helpers.LogErrorWithContext(context.Background(),
						"notification/workerLoop panic recovered: %v", r)
				}
			}()
			reapStale()
			processBatch(shutdownCtx)
		}()

		select {
		case <-shutdownCtx.Done():
			helpers.MessageLogs.InfoLog.Println("Email worker shutting down")
			return
		case <-EmailWorkerSignal:
			// New row enqueued; loop immediately.
		case <-ticker.C:
			// Periodic check for deferred items (quiet hours, retries).
		}
	}
}

func cleanupLoop(shutdownCtx context.Context) {
	// Run once shortly after start, then every 24h.
	time.Sleep(2 * time.Minute)
	for {
		runCleanup()
		select {
		case <-shutdownCtx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

func runCleanup() {
	if n, err := emailQueueModels.CleanupOld(retentionTerminalRows); err == nil && n > 0 {
		helpers.MessageLogs.InfoLog.Printf("Email worker cleanup: removed %d terminal queue rows", n)
	}
	if n, err := emailLogModels.CleanupOld(retentionLogRows); err == nil && n > 0 {
		helpers.MessageLogs.InfoLog.Printf("Email worker cleanup: removed %d log rows", n)
	}
}

func reapStale() {
	if n, err := emailQueueModels.ReapStale(staleAfter); err == nil && n > 0 {
		helpers.MessageLogs.InfoLog.Printf("Email worker reaped %d stale processing rows", n)
	}
}

func processBatch(shutdownCtx context.Context) {
	items, err := emailQueueModels.FetchAndLockPending(batchSize)
	if err != nil || len(items) == 0 {
		return
	}

	concurrency := concurrencyDefault
	if v := os.Getenv("EMAIL_WORKER_CONCURRENCY"); v != "" {
		var n int
		if _, e := fmt.Sscanf(v, "%d", &n); e == nil && n > 0 && n <= 20 {
			concurrency = n
		}
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(item *emailQueueModels.QueueItem) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					helpers.LogErrorWithContext(context.Background(),
						"notification/processBatch worker panic recovered: %v", r)
					// Best-effort: leave the row in 'processing'; the reaper
					// will roll it back to 'pending' after staleAfter.
				}
			}()
			deliverItem(shutdownCtx, item)
		}(it)
	}
	wg.Wait()
}

// deliverItem performs the actual send for one queue row, then records the
// outcome in the log table and transitions the queue row.
func deliverItem(parentCtx context.Context, item *emailQueueModels.QueueItem) {
	// Pre-flight suppression check. Worth re-checking here because the
	// recipient may have been added to the suppression list between
	// enqueue and send.
	if reason, _ := suppressionModels.IsSuppressed(item.ToEmail); reason != "" {
		_ = emailQueueModels.MarkSuppressed(item.ID, "suppressed: "+reason)
		_ = emailLogModels.Insert(emailLogModels.LogRow{
			UserID:    &item.UserID,
			ToEmail:   item.ToEmail,
			EventType: item.EventType,
			Subject:   item.Subject,
			Status:    emailLogModels.StatusSuppressed,
			QueueID:   &item.ID,
			Attempts:  item.Attempts,
		})
		return
	}

	// Per-call timeout — don't let a single slow Resend response stall the
	// worker. The token-bucket inside emailService also enforces a global
	// rate ceiling.
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	senderEmail := resolveSender()

	res, err := emailService.SendEmailWithOptions(ctx, emailService.SendOptions{
		From:                senderEmail,
		To:                  item.ToEmail,
		Subject:             item.Subject,
		HTML:                item.HTMLBody,
		Text:                item.TextBody,
		IdempotencyKey:      item.DedupKey,
		UnsubscribeURL:      buildUnsubscribeFromQueue(item),
		ListUnsubscribePost: true,
		Tags: map[string]string{
			"event_type": item.EventType,
		},
	})

	if errors.Is(err, emailService.ErrUndeliverable) {
		// Nothing was sent, and nothing ever could be: suppressed, not failed.
		_ = emailQueueModels.MarkSuppressed(item.ID, "suppressed: "+err.Error())
		_ = emailLogModels.Insert(emailLogModels.LogRow{
			UserID:    &item.UserID,
			ToEmail:   item.ToEmail,
			EventType: item.EventType,
			Subject:   item.Subject,
			Status:    emailLogModels.StatusSuppressed,
			QueueID:   &item.ID,
			Attempts:  item.Attempts,
		})
		return
	}
	if err != nil {
		errMsg := strings.TrimSpace(err.Error())
		if len(errMsg) > 1000 {
			errMsg = errMsg[:1000]
		}
		if emailService.IsTerminal(err) || item.Attempts >= maxAttempts {
			_ = emailQueueModels.MarkFailed(item.ID, errMsg)
			_ = emailLogModels.Insert(emailLogModels.LogRow{
				UserID:       &item.UserID,
				ToEmail:      item.ToEmail,
				EventType:    item.EventType,
				Subject:      item.Subject,
				Status:       emailLogModels.StatusFailed,
				ErrorMessage: &errMsg,
				QueueID:      &item.ID,
				Attempts:     item.Attempts,
			})
			return
		}
		// Transient: exponential backoff with jitter capped at 10 minutes.
		// Attempts has already been incremented by FetchAndLockPending.
		// Jitter prevents thundering herds when Resend recovers from a
		// rate-limit window with many of our rows ready at the same instant.
		base := time.Duration(1<<uint(item.Attempts-1)) * 30 * time.Second
		if base > 10*time.Minute {
			base = 10 * time.Minute
		}
		jitter := time.Duration(rand.Int63n(int64(base / 4)))
		nextRetry := time.Now().Add(base + jitter)
		_ = emailQueueModels.RetryLater(item.ID, errMsg, nextRetry)
		return
	}

	// Success.
	_ = emailQueueModels.MarkSent(item.ID)
	var providerID *string
	if res.MessageID != "" {
		mid := res.MessageID
		providerID = &mid
	}
	_ = emailLogModels.Insert(emailLogModels.LogRow{
		UserID:            &item.UserID,
		ToEmail:           item.ToEmail,
		EventType:         item.EventType,
		Subject:           item.Subject,
		Status:            emailLogModels.StatusSent,
		ProviderMessageID: providerID,
		QueueID:           &item.ID,
		Attempts:          item.Attempts,
	})
}

// resolveSender pulls the configured sender email from system_configs, with
// a sane fallback. Cached behaviour is provided by the Postgres cache; one
// query per email is fine at expected volumes.
func resolveSender() string {
	cfg, err := configModels.GetConfigByKey("sender_email")
	if err == nil && cfg != nil && strings.TrimSpace(cfg.Value) != "" {
		return cfg.Value
	}
	return "noreply@onemana.dev"
}

// buildUnsubscribeFromQueue reconstructs the unsubscribe URL for a queue row.
//
// The token was captured into metadata at enqueue time, so this is a pure
// function of the row — no extra Postgres roundtrip per send. We fall back
// to an empty header (no List-Unsubscribe) when the metadata is missing or
// malformed; the email is still delivered, just without the one-click link.
func buildUnsubscribeFromQueue(it *emailQueueModels.QueueItem) string {
	if len(it.Metadata) == 0 {
		return ""
	}
	var meta struct {
		UnsubscribeToken string `json:"unsubscribe_token"`
	}
	if err := json.Unmarshal(it.Metadata, &meta); err != nil {
		return ""
	}
	if meta.UnsubscribeToken == "" {
		return ""
	}
	return buildUnsubscribeURL(meta.UnsubscribeToken)
}
