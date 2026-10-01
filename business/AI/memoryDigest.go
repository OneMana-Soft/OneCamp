package business

// Memory-aware proactive digest.
//
// WHY
// ---
// The memory layer already answers "what's still open / who owns what" when
// asked. The digest flips that from pull to PUSH: once a day it finds each
// person's overdue commitments and stalled open questions and emails them a
// short, personal "open items" nudge. This is the kind of compounding,
// workspace-aware automation a chat-only tool can't do — it requires owning
// the structured knowledge AND the delivery channel.
//
// PRODUCTION PROPERTIES
//   - Reuses the existing notification pipeline (Dispatch), so it inherits
//     suppression, quiet-hours, unsubscribe, dedup, and the email queue +
//     retry/backoff worker for free. The digest sets SkipOnlineCheck (email
//     is the primary channel) and is deduped per (recipient, day).
//   - Self-gating: no-op unless AI + memory layer are enabled, and a no-op
//     when email is unconfigured (Dispatch short-circuits).
//   - Respects each user's email_digest_frequency ("off"/"daily"/"weekly")
//     — "off" recipients are skipped; "weekly" only fires on the configured
//     weekday.
//   - One DB query for ALL actionable items, grouped in-memory by owner, so
//     a large workspace costs O(1) queries, not O(users).
//   - Bounded: per-recipient item count is capped; the body is plain text
//     (the email template renders Body as text).
//   - Runs at a configurable local hour; a per-day Redis lock makes the
//     daily run idempotent across overlapping ticks / instances.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	notification "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	// Digest cadence check interval. The worker wakes hourly and only acts
	// when the local hour matches the configured send hour (and the per-day
	// lock is free), so the actual send happens once/day.
	digestTickInterval = 1 * time.Hour

	// Default local hour-of-day to send (08:00). Tunable via
	// AI_MEMORY_DIGEST_HOUR (0-23).
	digestDefaultHour = 8

	// A commitment is "overdue" once its due date passed. A question/commit
	// is "stale" once untouched for this long.
	digestStaleAfter = 7 * 24 * time.Hour

	// Max items shown per recipient in one digest (keeps the email short and
	// the LLM-free path cheap).
	maxDigestItemsPerUser = 12

	// Safety cap on total actionable rows scanned per run.
	maxDigestRowsPerRun = 4000

	// Initial delay after boot before the first cadence check.
	digestInitialDelay = 15 * time.Minute
)

// StartMemoryDigestLoop launches the proactive digest worker. Safe to call
// once at startup; it self-gates on settings every tick and is inert until
// the memory layer is enabled (and email configured). ctx drives shutdown.
func StartMemoryDigestLoop(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(digestInitialDelay):
		}
		ticker := time.NewTicker(digestTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				helpers.MessageLogs.InfoLog.Println("Memory digest worker shutting down")
				return
			case <-ticker.C:
				maybeRunDigest(ctx)
			}
		}
	}()
}

// maybeRunDigest fires the digest exactly once on the configured local hour
// per day. The per-day lock guards against multiple ticks within the send
// hour and multiple instances.
func maybeRunDigest(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("panic in memory digest: %v", r)
		}
	}()

	now := time.Now()
	if now.Hour() != digestSendHour() {
		return
	}

	settings, err := getAISettingsForMemory(ctx)
	if err != nil || !settings.enabled || !settings.memoryEnabled {
		return
	}
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}

	// Idempotency: one digest run per calendar day.
	dayKey := now.Format("2006-01-02")
	if res := redisStore.AllowFixedWindow(ctx, registry.AIMemoryDigestLock, []string{dayKey}, 1); !res.Allowed {
		return
	}

	runDigest(ctx, now)
}

// runDigest builds and dispatches per-user digests for the current day.
func runDigest(ctx context.Context, now time.Time) {
	overdueBefore := now
	staleBefore := now.Add(-digestStaleAfter)

	items, err := memoryModels.ListActionableForDigest(ctx, overdueBefore, staleBefore, maxDigestRowsPerRun)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory digest: query actionable items failed: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}

	// Group by owner.
	byOwner := make(map[uuid.UUID][]*memoryModels.MemoryItem)
	for _, di := range items {
		byOwner[di.OwnerID] = append(byOwner[di.OwnerID], di.Item)
	}

	// Resolve which owners want a digest today in one batched prefs lookup.
	ownerIDs := make([]uuid.UUID, 0, len(byOwner))
	for id := range byOwner {
		ownerIDs = append(ownerIDs, id)
	}
	prefs, err := prefModels.GetByUserIDs(ownerIDs)
	if err != nil {
		// Fail open is wrong for a push channel (could spam); fail closed.
		helpers.LogErrorWithContext(ctx, "memory digest: prefs lookup failed: %v", err)
		return
	}

	weekday := now.Weekday()
	sent := 0
	for ownerID, list := range byOwner {
		pref := prefs[ownerID]
		if !digestWantedToday(pref, weekday) {
			continue
		}
		subject, body := buildDigest(list)
		if body == "" {
			continue
		}
		notification.DispatchMemoryDigest(ownerID.String(), subject, body)
		sent++
	}
	helpers.LogInfoWithContext(ctx, "memory digest: dispatched to %d user(s)", sent)
}

// digestWantedToday decides whether the recipient should get a digest now,
// based on their email_digest_frequency. Missing prefs row → no digest
// (user hasn't opted in). "daily" → every day; "weekly" → Mondays only.
// "off"/"" → never. email_enabled is re-checked downstream by Dispatch.
func digestWantedToday(pref *prefModels.UserNotificationPreference, weekday time.Weekday) bool {
	if pref == nil || !pref.EmailEnabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(pref.EmailDigestFrequency)) {
	case "daily":
		return true
	case "weekly":
		return weekday == time.Monday
	default:
		return false
	}
}

// buildDigest renders a recipient's actionable items into (subject, body).
// Body is plain text (the email template renders it as text). Overdue
// commitments come first, then stale open questions; bounded by
// maxDigestItemsPerUser.
func buildDigest(items []*memoryModels.MemoryItem) (string, string) {
	// Stable ordering: overdue commitments (by due date) first, then
	// questions/other (by recency of content creation).
	sort.SliceStable(items, func(i, j int) bool {
		oi, oj := digestRank(items[i]), digestRank(items[j])
		if oi != oj {
			return oi < oj
		}
		if items[i].DueAt != nil && items[j].DueAt != nil {
			return items[i].DueAt.Before(*items[j].DueAt)
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})

	if len(items) > maxDigestItemsPerUser {
		items = items[:maxDigestItemsPerUser]
	}

	var commitments, questions []string
	for _, it := range items {
		line := digestLine(it)
		switch it.Kind {
		case memoryModels.KindCommitment:
			commitments = append(commitments, line)
		case memoryModels.KindQuestion:
			questions = append(questions, line)
		default:
			questions = append(questions, line)
		}
	}

	var sb strings.Builder
	if len(commitments) > 0 {
		sb.WriteString("Overdue / due commitments:\n")
		for _, l := range commitments {
			sb.WriteString("• " + l + "\n")
		}
		sb.WriteString("\n")
	}
	if len(questions) > 0 {
		sb.WriteString("Open questions that need attention:\n")
		for _, l := range questions {
			sb.WriteString("• " + l + "\n")
		}
	}
	body := strings.TrimSpace(sb.String())
	if body == "" {
		return "", ""
	}

	total := len(commitments) + len(questions)
	subject := fmt.Sprintf("You have %d open item%s in OneCamp", total, plural(total))
	return subject, body
}

// digestRank orders kinds: overdue commitments (0), other commitments (1),
// questions (2).
func digestRank(it *memoryModels.MemoryItem) int {
	if it.Kind == memoryModels.KindCommitment {
		if it.DueAt != nil {
			return 0
		}
		return 1
	}
	return 2
}

// digestLine renders one item as a compact plain-text line with optional
// due-date suffix.
func digestLine(it *memoryModels.MemoryItem) string {
	line := strings.TrimSpace(it.Content)
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	if it.DueAt != nil {
		line += " (due " + it.DueAt.Format("Jan 2") + ")"
	}
	return line
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// SendTestMemoryDigest builds the requesting admin's current open-items digest
// and emails it to them immediately, bypassing the daily schedule, the per-day
// lock, and the per-user frequency preference, so an admin can verify email
// delivery. If they have no actionable items, a short confirmation is sent
// instead (delivery is always testable). The recipient's email_enabled and
// suppression rules are still honored by Dispatch.
func SendTestMemoryDigest(ctx context.Context, userUUID string) error {
	uid, err := uuid.Parse(strings.TrimSpace(userUUID))
	if err != nil {
		return fmt.Errorf("invalid user id")
	}
	if settings, serr := getAISettingsForMemory(ctx); serr != nil || !settings.enabled || !settings.memoryEnabled {
		return fmt.Errorf("enable Workspace AI and Workspace Memory first")
	}

	now := time.Now()
	items, err := memoryModels.ListActionableForDigest(ctx, now, now.Add(-digestStaleAfter), maxDigestRowsPerRun)
	if err != nil {
		return fmt.Errorf("load actionable items: %w", err)
	}
	var mine []*memoryModels.MemoryItem
	for _, di := range items {
		if di.OwnerID == uid {
			mine = append(mine, di.Item)
		}
	}

	subject, body := buildDigest(mine)
	if body == "" {
		subject = "OneCamp digest - test"
		body = "This is a test of your OneCamp open-items digest. You have no overdue commitments or stale open questions right now, so a normal digest would not be sent - but email delivery is working."
	} else {
		subject = "[Test] " + subject
	}
	// Synchronous, and the result is CHECKED. This is a test button: reporting
	// success without knowing is how a broken email queue looked like a working
	// one for as long as nobody read the server log.
	if sent := notification.DispatchMemoryDigestTestNow(ctx, uid.String(), subject, body); sent == 0 {
		return fmt.Errorf("the digest could not be sent. Check that email is configured for this workspace, " +
			"that your account has an email address, and that email notifications are enabled in your settings")
	}
	return nil
}

// digestSendHour reads AI_MEMORY_DIGEST_HOUR (0-23), defaulting to
// digestDefaultHour. Unset or out-of-range falls back to the default.
func digestSendHour() int {
	v := strings.TrimSpace(os.Getenv("AI_MEMORY_DIGEST_HOUR"))
	if v == "" {
		return digestDefaultHour
	}
	h, err := strconv.Atoi(v)
	if err != nil || h < 0 || h > 23 {
		return digestDefaultHour
	}
	return h
}
