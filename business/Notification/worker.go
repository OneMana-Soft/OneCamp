package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
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

	helpers.GoSafeLoop("workerLoop", func() { workerLoop(shutdownCtx) })
	helpers.GoSafeLoop("cleanupLoop", func() { cleanupLoop(shutdownCtx) })
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

// sendEmail is the one send of a queue row's email; an in-package test puts
// a recorder in its place to see what the worker would send.
var sendEmail = emailService.SendEmailWithOptions

// retryLater leaves queue rows waiting, saying why, until at; a seam, as
// sendEmail is.
var retryLater = emailQueueModels.RetryLater

// processBatch claims what is due, with each batched row's window-mates, and
// settles it.
func processBatch(shutdownCtx context.Context) {
	items, err := emailQueueModels.FetchAndLockPending(batchSize, claimMates)
	if err != nil || len(items) == 0 {
		return
	}
	settle(shutdownCtx, items)
}

// settle asks every claimed row again (recheck.go) and sends what still
// holds: a person's batched rows as one email (batch.go), any other row as
// its own. Each outcome is one UPDATE over its rows.
func settle(shutdownCtx context.Context, items []*emailQueueModels.QueueItem) {
	held := verdicts(shutdownCtx, items)
	var live, gone, later []*emailQueueModels.QueueItem
	for _, it := range items {
		switch held[it.ID] {
		case noLongerTold:
			gone = append(gone, it)
		case askLater:
			later = append(later, it)
		default:
			live = append(live, it)
		}
	}
	suppress(gone, "no longer told")
	_ = retryLater(emailQueueModels.IDsOf(later), "what it is about couldn't be read", time.Now().Add(time.Minute))
	groups := groupsOf(live)
	if len(groups) == 0 {
		return
	}

	// Once for the batch: who is on the suppression list now (they may have
	// joined it since the enqueue), the sender, and the logo a list shows.
	var addresses []string
	for _, g := range groups {
		addresses = append(addresses, g[0].ToEmail)
	}
	blocked, _ := suppressionModels.IsSuppressedBatch(addresses)
	sh := shell{sender: resolveSender()}
	if slices.ContainsFunc(groups, func(g group) bool { return len(g) > 1 }) {
		sh.logo = brandLogoURL()
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

	for _, g := range groups {
		wg.Add(1)
		sem <- struct{}{}
		go func(g group) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					helpers.LogErrorWithContext(context.Background(),
						"notification/processBatch worker panic recovered: %v", r)
					// Best-effort: leave the rows in 'processing'; the reaper
					// will roll them back to 'pending' after staleAfter.
				}
			}()
			deliverGroup(shutdownCtx, g, sh, blocked[strings.ToLower(strings.TrimSpace(g[0].ToEmail))])
		}(g)
	}
	wg.Wait()
}

// deliverGroup sends g's one email, unless its address is on the suppression
// list (blocked says why), then writes its rows' outcome and its log row
// right after the send, so only a crash between the two can send it again,
// and then under the same key (groupsOf), which the provider sends once.
func deliverGroup(parentCtx context.Context, g group, sh shell, blocked string) {
	if blocked != "" {
		suppress(g, blocked)
		return
	}
	m, err := g.mail(sh)
	if err != nil {
		helpers.LogErrorWithContext(parentCtx, "notification/deliverGroup render failed: %+v", err)
		errMsg := "render failed: " + err.Error()
		_ = emailQueueModels.MarkFailed(g.ids(), errMsg)
		logEmail(g, m, emailLogModels.StatusFailed, nil, &errMsg)
		return
	}

	// Per-call timeout — don't let a single slow Resend response stall the
	// worker. The token-bucket inside emailService also enforces a global
	// rate ceiling.
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	res, err := sendEmail(ctx, g.options(m, sh))

	if errors.Is(err, emailService.ErrUndeliverable) {
		// Nothing was sent, and nothing ever could be: suppressed, not failed.
		suppress(g, err.Error())
		return
	}
	if err != nil {
		errMsg := strings.TrimSpace(err.Error())
		if len(errMsg) > 1000 {
			errMsg = errMsg[:1000]
		}
		attempts := g.attempts()
		if emailService.IsTerminal(err) || attempts >= maxAttempts {
			_ = emailQueueModels.MarkFailed(g.ids(), errMsg)
			logEmail(g, m, emailLogModels.StatusFailed, nil, &errMsg)
			return
		}
		// Transient: exponential backoff with jitter capped at 10 minutes.
		// Attempts has already been incremented by FetchAndLockPending.
		// Jitter prevents thundering herds when Resend recovers from a
		// rate-limit window with many of our rows ready at the same instant.
		base := time.Duration(1<<uint(attempts-1)) * 30 * time.Second
		if base > 10*time.Minute {
			base = 10 * time.Minute
		}
		jitter := time.Duration(rand.Int63n(int64(base / 4)))
		_ = retryLater(g.ids(), errMsg, time.Now().Add(base+jitter))
		return
	}

	// Success.
	_ = emailQueueModels.MarkSent(g.ids())
	var providerID *string
	if res.MessageID != "" {
		mid := res.MessageID
		providerID = &mid
	}
	logEmail(g, m, emailLogModels.StatusSent, providerID, nil)
}

// logEmail writes g's one email to the log, under its newest row.
func logEmail(g group, m mail, status string, providerID, errMsg *string) {
	_ = emailLogModels.Insert(emailLogModels.LogRow{
		UserID:            &g[0].UserID,
		ToEmail:           g[0].ToEmail,
		EventType:         m.kind,
		Subject:           m.subject,
		Status:            status,
		ProviderMessageID: providerID,
		ErrorMessage:      errMsg,
		QueueID:           &g[0].ID,
		Attempts:          g.attempts(),
	})
}

// suppress records that items' emails are not sent, and why: one UPDATE
// over their rows, and a log row each, in one INSERT.
func suppress(items []*emailQueueModels.QueueItem, why string) {
	if len(items) == 0 {
		return
	}
	_ = emailQueueModels.MarkSuppressed(emailQueueModels.IDsOf(items), "suppressed: "+why)
	rows := make([]emailLogModels.LogRow, len(items))
	for i, it := range items {
		rows[i] = emailLogModels.LogRow{
			UserID:    &it.UserID,
			ToEmail:   it.ToEmail,
			EventType: it.EventType,
			Subject:   it.Subject,
			Status:    emailLogModels.StatusSuppressed,
			QueueID:   &it.ID,
			Attempts:  it.Attempts,
		}
	}
	_ = emailLogModels.InsertMany(rows)
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
