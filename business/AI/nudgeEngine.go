package business

// AI Proactive Nudges — the "push" arm of the workspace AI.
//
// WHY THIS EXISTS
// ---------------
// The memory layer and connectors answer questions when ASKED. Nudges flip
// that to PUSH: a periodic sweep evaluates workspace state and surfaces a
// short, actionable prompt to the responsible user — "you committed to X by
// Friday and it's Thursday", "this question has been open a week", "you have
// PRs waiting on your review". This is the single feature that makes the
// product feel alive rather than inert.
//
// HOW IT STAYS CHEAP AND CORRECT
//   - Fully gated on ai_settings (AI enabled + nudges_enabled); a workspace
//     that never turns it on pays nothing.
//   - One batched DB query (ListActionableForDigest) yields actionable memory
//     items across ALL owners — O(1) queries, grouped in-memory by owner.
//   - Each candidate nudge has a stable dedup_key; Upsert keeps at most one
//     OPEN nudge per logical signal, so re-runs refresh rather than duplicate.
//   - A recently dismissed/acted signal is NOT re-surfaced (HasRecentTerminal),
//     so the user isn't nagged about something they just handled.
//   - At the end of a user's pass, open nudges whose signal no longer holds are
//     superseded — a resolved commitment's nudge disappears on its own.
//   - A per-window Redis lock makes the run idempotent across overlapping ticks
//     and multiple instances.
//   - LLM phrasing is OPTIONAL and circuit-breaker guarded; if AI is slow/down
//     we fall back to a deterministic template, so a nudge never blocks on the
//     model.
//
// Delivery is dual: a persisted workspace_nudges row (so the bell shows it on
// next load) AND a real-time MQTT publish to the user's activity topic (so the
// badge updates live). Email is intentionally NOT used here — the daily memory
// digest already owns the email channel; nudges are the in-app, real-time layer.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	nudgeModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	// Stagger after boot so the engine doesn't contend with the memory
	// extraction / digest workers; nudges are never time-critical.
	nudgeInitialDelay = 12 * time.Minute

	// Default cadence between sweeps. Tunable via AI_NUDGE_INTERVAL_MIN.
	nudgeDefaultInterval = 30 * time.Minute

	// A commitment is overdue once its due date passed; a commitment/question
	// is "stale" once untouched for this long.
	nudgeStaleAfter = 7 * 24 * time.Hour

	// Don't re-surface a signal the user dismissed/acted on within this window.
	nudgeSuppressAfterTerminal = 3 * 24 * time.Hour

	// Bound rows scanned and nudges generated per user per run.
	nudgeMaxRows    = 4000
	nudgeMaxPerUser = 10

	// Terminal nudges older than this are purged.
	nudgePurgeAfter = 30 * 24 * time.Hour

	// Optional LLM phrasing budget. Off by default to stay cheap; flip on via
	// AI_NUDGE_LLM_PHRASING=true.
	nudgePhrasingPrompt = `Rewrite the following workspace reminder as ONE short, friendly, action-oriented sentence (max 18 words). No preamble, no quotes, no markdown. Reminder: `
)

// StartNudgeLoop launches the proactive-nudge engine. Safe to call once at
// startup; it self-gates on settings every tick and is inert until an admin
// enables nudges. ctx drives graceful shutdown.
func StartNudgeLoop(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(nudgeInitialDelay):
		}
		ticker := time.NewTicker(nudgeInterval())
		defer ticker.Stop()

		runNudgePass(ctx)
		for {
			select {
			case <-ctx.Done():
				helpers.MessageLogs.InfoLog.Println("Nudge engine shutting down")
				return
			case <-ticker.C:
				runNudgePass(ctx)
			}
		}
	}()
}

// nudgeInterval reads AI_NUDGE_INTERVAL_MIN, defaulting to nudgeDefaultInterval.
func nudgeInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("AI_NUDGE_INTERVAL_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return nudgeDefaultInterval
}

// runNudgePass performs a single sweep: gate → lock → evaluate rules per owner
// → upsert + publish → supersede stale → purge. Best-effort throughout.
func runNudgePass(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("panic in nudge pass: %v", r)
		}
	}()

	settings, err := getAISettingsForMemory(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: load settings failed: %v", err)
		return
	}
	if !settings.enabled || !settings.nudgesEnabled {
		return // feature off — silent no-op
	}
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}

	// Idempotency: one run per coarse window across ticks/instances.
	windowKey := time.Now().Format("2006-01-02T15") // hour bucket
	if res := redisStore.AllowFixedWindow(ctx, registry.AINudgeRunLock, []string{windowKey}, 1); !res.Allowed {
		return
	}

	now := time.Now()
	candidates := gatherMemoryNudges(ctx, now)

	// Group by owner so we can cap per user and supersede stale in one shot.
	byUser := map[uuid.UUID][]candidateNudge{}
	for _, c := range candidates {
		byUser[c.userID] = append(byUser[c.userID], c)
	}

	totalCreated := 0
	for userID, list := range byUser {
		select {
		case <-ctx.Done():
			return
		default:
		}
		created := applyUserNudges(ctx, userID, list)
		totalCreated += created
	}

	// Reconcile users whose signals ALL resolved since the last pass: they
	// produce no candidates this run, so they're absent from byUser and their
	// stale open nudges would otherwise linger. Supersede all open nudges for
	// any user with open nudges who had no candidate this pass.
	reconcileResolvedUsers(ctx, byUser)

	// Housekeeping: purge old terminal rows. Best-effort.
	if n, perr := nudgeModels.PurgeOldTerminal(ctx, nudgePurgeAfter); perr == nil && n > 0 {
		helpers.LogInfoWithContext(ctx, "nudge engine: purged %d old terminal nudge(s)", n)
	}

	helpers.LogInfoWithContext(ctx, "nudge engine: evaluated %d owner(s), created/refreshed %d nudge(s)", len(byUser), totalCreated)
}

// reconcileResolvedUsers supersedes open nudges for users who had open nudges
// but produced zero candidates this pass (every signal resolved). Without this,
// a nudge for a commitment that got done would never clear on its own.
func reconcileResolvedUsers(ctx context.Context, byUser map[uuid.UUID][]candidateNudge) {
	openUsers, err := nudgeModels.UsersWithOpenNudges(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: list open-nudge users failed: %v", err)
		return
	}
	for _, uid := range openUsers {
		if _, hadCandidates := byUser[uid]; hadCandidates {
			continue // already reconciled in applyUserNudges via its keep-set
		}
		if n, serr := nudgeModels.SupersedeStaleForUser(ctx, uid, nil); serr == nil && n > 0 {
			publishBadge(ctx, uid)
		}
	}
}

// candidateNudge is a pre-delivery nudge with its dedup key + priority.
type candidateNudge struct {
	userID     uuid.UUID
	kind       string
	title      string
	body       string
	ctaURL     string
	ctaText    string
	sourceType string
	sourceID   string
	priority   int
	dedupKey   string
}

// gatherMemoryNudges turns actionable memory items (overdue commitments + stale
// open questions) into candidate nudges. This is the first rule source; more
// (blocked tasks, unreviewed PRs) can be appended here following the same shape.
func gatherMemoryNudges(ctx context.Context, now time.Time) []candidateNudge {
	overdueBefore := now
	staleBefore := now.Add(-nudgeStaleAfter)

	items, err := memoryModels.ListActionableForDigest(ctx, overdueBefore, staleBefore, nudgeMaxRows)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: actionable items query failed: %v", err)
		return nil
	}

	// PRIVACY GATE: ListActionableForDigest selects on owner_user_id only — an
	// item's owner was a scope member when it was captured, but membership
	// changes. Before pushing a nudge we re-verify the owner is STILL in the
	// item's channel/project/DM scope. Otherwise a nudge (with the item's
	// content) would follow a user out of a conversation they've left, leaking
	// it. When we can positively confirm the owner left, we also clear
	// owner_user_id so future passes don't re-scan it (self-healing).
	memberCache := map[string]*dgraphMembership{}

	var out []candidateNudge
	for _, di := range items {
		it := di.Item
		inScope, resolved := ownerScopeStatus(ctx, di.OwnerID, it, memberCache)
		if !inScope {
			// Only DETACH when we positively resolved the owner's profile and
			// confirmed they're no longer in scope. On a transient lookup
			// failure (resolved == false) we skip this pass but keep ownership
			// intact, so a Dgraph blip can't orphan items.
			if resolved {
				if cerr := memoryModels.ClearOwner(ctx, it.ID); cerr != nil {
					helpers.LogErrorWithContext(ctx, "nudge engine: clear stale owner for %s failed: %v", it.ID, cerr)
				}
			}
			continue
		}
		c := candidateNudge{
			userID:     di.OwnerID,
			sourceType: "memory",
			sourceID:   it.ID.String(),
			// Deep-link straight to THIS item (the panel reads ?item= to switch
			// to the open view, scroll to, and highlight it) — not the generic
			// list — so clicking the nudge lands the user on the exact thing.
			ctaURL:   "/app/ai/memory?item=" + it.ID.String(),
			ctaText:  "View",
			dedupKey: "memory:" + it.ID.String(),
		}
		switch it.Kind {
		case memoryModels.KindCommitment:
			c.kind = nudgeModels.KindOverdueCommitment
			c.priority = 1
			due := ""
			if it.DueAt != nil {
				due = " (was due " + it.DueAt.Format("Jan 2") + ")"
			}
			c.title = "Overdue commitment"
			c.body = templateNudge(ctx, fmt.Sprintf("You committed to: %s%s.", trimOneLine(it.Content, 200), due))
		case memoryModels.KindQuestion:
			c.kind = nudgeModels.KindStaleQuestion
			c.priority = 0
			c.title = "Open question needs attention"
			c.body = templateNudge(ctx, fmt.Sprintf("This is still unresolved: %s", trimOneLine(it.Content, 200)))
		default:
			continue
		}
		out = append(out, c)
	}
	return out
}

// dgraphMembership is a per-owner snapshot of scope membership used to gate
// nudge delivery. Built lazily from the owner's Dgraph profile and cached for
// the duration of a single pass (one lookup per distinct owner).
type dgraphMembership struct {
	channels map[string]struct{}
	projects map[string]struct{}
	dmGroups map[string]struct{}
	// resolved is false when the owner's profile couldn't be loaded (deleted
	// user / transient error); callers treat that as "no longer in scope".
	resolved bool
}

// ownerScopeStatus reports whether the owner currently belongs to the item's
// scope (channel / project / DM-group), and whether the owner's profile was
// resolved at all. This is the fire-time complement to memoryItemVisibleTo:
// an item is only ever nudged to an owner who can still see where it lives.
// The owner's postgres Id equals their Dgraph user_uuid.
//
// Returns (inScope, resolved). When resolved is false the owner's profile
// couldn't be loaded (deleted user / transient error) and inScope is false —
// the caller must NOT clear ownership in that case, only skip the pass.
func ownerScopeStatus(ctx context.Context, ownerID uuid.UUID, it *memoryModels.MemoryItem, cache map[string]*dgraphMembership) (inScope bool, resolved bool) {
	key := ownerID.String()
	m, ok := cache[key]
	if !ok {
		m = loadOwnerMembership(ctx, key)
		cache[key] = m
	}
	if !m.resolved {
		return false, false
	}
	if it.ChannelUUID != nil {
		if _, in := m.channels[it.ChannelUUID.String()]; in {
			return true, true
		}
	}
	if it.ProjectUUID != nil {
		if _, in := m.projects[it.ProjectUUID.String()]; in {
			return true, true
		}
	}
	if it.ChatGrpID != "" {
		if _, in := m.dmGroups[it.ChatGrpID]; in {
			return true, true
		}
	}
	return false, true
}

// loadOwnerMembership fetches the owner's current Dgraph membership sets. A
// missing profile or error yields an unresolved snapshot so the caller fails
// closed (no nudge) rather than leaking.
func loadOwnerMembership(ctx context.Context, ownerUUID string) *dgraphMembership {
	m := &dgraphMembership{
		channels: map[string]struct{}{},
		projects: map[string]struct{}{},
		dmGroups: map[string]struct{}{},
	}
	info, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, ownerUUID)
	if err != nil || info == nil {
		return m // resolved == false
	}
	for _, ch := range info.Channels {
		if ch != nil && ch.Uuid != "" {
			m.channels[ch.Uuid] = struct{}{}
		}
	}
	for _, pr := range info.Projects {
		if pr != nil && pr.Uuid != "" {
			m.projects[pr.Uuid] = struct{}{}
		}
	}
	for _, dm := range info.DMs {
		if dm != nil && dm.GroupingId != "" {
			m.dmGroups[dm.GroupingId] = struct{}{}
		}
	}
	m.resolved = true
	return m
}

// applyUserNudges upserts a user's candidate nudges (capped, dedup-suppressed),
// publishes new ones live, then supersedes any of their open nudges whose
// signal no longer appears. Returns the count newly created.
func applyUserNudges(ctx context.Context, userID uuid.UUID, list []candidateNudge) int {
	// Highest priority first, then stable.
	sort.SliceStable(list, func(i, j int) bool { return list[i].priority > list[j].priority })

	// The supersede keep-set is EVERY currently-valid signal for this user —
	// computed from the full candidate list BEFORE the per-pass cap. If we
	// derived `keep` only from the capped/created subset, a user with more than
	// nudgeMaxPerUser live signals would have their still-valid open nudges
	// wrongly superseded just because they didn't make this pass's cap.
	keep := make([]string, 0, len(list))
	for _, c := range list {
		keep = append(keep, c.dedupKey)
	}

	// Cap how many we upsert/create per pass (bounds work + nudge volume); the
	// rest remain represented in `keep` so they're not superseded.
	work := list
	if len(work) > nudgeMaxPerUser {
		work = work[:nudgeMaxPerUser]
	}

	created := 0
	for _, c := range work {
		// Skip signals the user just handled (don't re-surface a dismissed one).
		if suppressed, _ := nudgeModels.HasRecentTerminal(ctx, c.dedupKey, nudgeSuppressAfterTerminal); suppressed {
			continue
		}

		id, inserted, err := nudgeModels.Upsert(ctx, nudgeModels.UpsertInput{
			UserID:     userID,
			Kind:       c.kind,
			Title:      c.title,
			Body:       c.body,
			CTAURL:     c.ctaURL,
			CTAText:    c.ctaText,
			SourceType: c.sourceType,
			SourceID:   c.sourceID,
			Priority:   c.priority,
			DedupKey:   c.dedupKey,
		})
		if err != nil {
			helpers.LogErrorWithContext(ctx, "nudge engine: upsert failed: %v", err)
			continue
		}
		if inserted {
			created++
			publishNudge(ctx, userID, id, c)
		}
	}

	// Supersede the user's open nudges whose signal no longer holds (resolved /
	// no longer overdue). If the user had open nudges and none survive, this
	// clears them — keeping the bell honest.
	if n, err := nudgeModels.SupersedeStaleForUser(ctx, userID, keep); err == nil && n > 0 {
		// A change to the open set: nudge the badge so it reflects reality.
		publishBadge(ctx, userID)
	}
	return created
}

// publishNudge sends a real-time "new nudge" event to the user's activity topic.
func publishNudge(ctx context.Context, userID, nudgeID uuid.UUID, c candidateNudge) {
	count, _ := nudgeModels.CountOpenForUser(ctx, userID)
	msg := &mqttStruct.MqttNudge{
		Action:    "new",
		NudgeID:   nudgeID.String(),
		Kind:      c.kind,
		Title:     c.title,
		Body:      c.body,
		CTAURL:    c.ctaURL,
		CTAText:   c.ctaText,
		Priority:  c.priority,
		OpenCount: count,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	PublishNudgeToUser(userID.String(), msg)
}

// publishBadge sends a lightweight "count changed" event (no new nudge).
func publishBadge(ctx context.Context, userID uuid.UUID) {
	count, _ := nudgeModels.CountOpenForUser(ctx, userID)
	PublishNudgeToUser(userID.String(), &mqttStruct.MqttNudge{Action: "cleared", OpenCount: count})
}

// ClearNudgeForMemoryItem supersedes the OPEN nudge for a memory item the
// instant its underlying signal is closed (resolved/dismissed/deleted), and
// pushes a live badge update so the bell clears immediately instead of waiting
// for the next ~30-min sweep. Best-effort: a failure just defers the clear to
// the next sweep (which supersedes stale signals anyway). Safe to call async.
func ClearNudgeForMemoryItem(ctx context.Context, memoryItemID string) {
	dedupKey := "memory:" + memoryItemID
	userID, err := nudgeModels.SupersedeOpenByDedupKey(ctx, dedupKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "ClearNudgeForMemoryItem supersede failed for %s: %v", memoryItemID, err)
		return
	}
	if userID == uuid.Nil {
		return // no open nudge for this item — nothing to clear
	}
	publishBadge(ctx, userID)
}

// PublishNudgeToUser publishes a nudge event to a single user's activity topic.
// Exported so the controller (a different package) can push live updates after
// dismiss/act actions.
func PublishNudgeToUser(userUUID string, msg *mqttStruct.MqttNudge) {
	mqttBusiness.PublishMessageToUser(userUUID, mqttStruct.MESSAGE_AI_NUDGE, msg)
}

// PublishNudgeBadge pushes a lightweight "open count changed" event to a user's
// activity topic. Exported for the controller to call after dismiss/act so all
// of a user's open tabs update their badge in real time.
func PublishNudgeBadge(userUUID string, openCount int) {
	PublishNudgeToUser(userUUID, &mqttStruct.MqttNudge{Action: "cleared", OpenCount: openCount})
}

// SetNudgesEnabled toggles the proactive-nudge engine (admin). Exported so the
// AI controller can flip it without importing the model package directly.
func SetNudgesEnabled(ctx context.Context, enabled bool) error {
	return aiModels.SetNudgesEnabled(ctx, enabled)
}

// templateNudge returns deterministic copy, optionally rewritten by the LLM
// when AI_NUDGE_LLM_PHRASING=true. LLM failures fall back to the template, so a
// nudge never blocks on the model.
func templateNudge(ctx context.Context, base string) string {
	if !llmPhrasingEnabled() {
		return base
	}
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return base
	}
	if err := svc.Resiliency.CB.Allow(); err != nil {
		return base
	}
	out, err := svc.Summarize(ctx, base, nudgePhrasingPrompt)
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		return base
	}
	svc.Resiliency.CB.RecordSuccess()
	out = strings.TrimSpace(out)
	if out == "" || len(out) > 240 {
		return base
	}
	return out
}

func llmPhrasingEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("AI_NUDGE_LLM_PHRASING")), "true")
}

// trimOneLine collapses whitespace and bounds length for a clean single line.
func trimOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
