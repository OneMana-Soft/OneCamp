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
	"maps"
	"os"
	"slices"
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
	helpers.GoSafeLoop("business/AI/nudgeEngine:StartNudgeLoop", func() {
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
	})
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

	// Open nudges are settled first, whether or not this pass makes any.
	settleOpenNudges(ctx, time.Now())

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

// nudgeQuietAfter is how long an urgent nudge stays urgent without being
// raised again: then it drops to priority 0, still listed.
const nudgeQuietAfter = 14 * 24 * time.Hour

// settleOpenNudges settles the open nudges a pass would otherwise leave as
// they were (V-581, D2), by a status or priority change, never a delete:
//   - an overdue-commitment nudge whose commitment no longer needs its person
//     now (done, deleted, missing, someone else's, or stale) is superseded,
//     by the same predicate the attention list uses (commitmentNeedsMeNow);
//   - any urgent nudge not raised again for nudgeQuietAfter drops to priority
//     0: still listed, no longer urgent. When it was last raised is left as
//     it was.
//
// Each person whose open nudges changed hears ONE "cleared" naming every
// nudge of theirs it superseded (none when theirs only went quiet), with
// their open count after, read for them all at once. Best effort: a failed
// read leaves the nudges for the next pass.
func settleOpenNudges(ctx context.Context, now time.Time) {
	// closed is each changed person's superseded nudges.
	closed := map[uuid.UUID][]uuid.UUID{}
	open, err := nudgeModels.ListOpenOfKind(ctx, nudgeModels.KindOverdueCommitment, nudgeMaxRows)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: list open commitment nudges failed: %v", err)
	} else if len(open) > 0 {
		var ids []uuid.UUID
		for _, n := range open {
			if id, perr := uuid.Parse(n.SourceID); perr == nil {
				ids = append(ids, id)
			}
		}
		live, lerr := memoryModels.LiveByIDs(ctx, ids)
		if lerr != nil {
			helpers.LogErrorWithContext(ctx, "nudge engine: read the commitments behind %d nudge(s) failed: %v", len(ids), lerr)
		} else {
			var settle []uuid.UUID
			for _, n := range open {
				id, _ := uuid.Parse(n.SourceID)
				it := live[id]
				if it == nil || it.OwnerID == nil || *it.OwnerID != n.UserID || !commitmentNeedsMeNow(it, now) {
					settle = append(settle, n.ID)
				}
			}
			byUser, serr := nudgeModels.SupersedeByIDs(ctx, settle)
			if serr != nil {
				helpers.LogErrorWithContext(ctx, "nudge engine: settle %d nudge(s) failed: %v", len(settle), serr)
			}
			maps.Copy(closed, byUser)
		}
	}
	quiet, err := nudgeModels.QuietUnraisedSince(ctx, now.Add(-nudgeQuietAfter))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: quiet unraised nudges failed: %v", err)
	}
	for _, u := range quiet {
		if _, ok := closed[u]; !ok {
			closed[u] = nil
		}
	}
	if len(closed) == 0 {
		return
	}
	counts, err := nudgeModels.CountOpenForUsers(ctx, slices.Collect(maps.Keys(closed)))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: count the open nudges of %d person(s) failed: %v", len(closed), err)
	}
	for u, ids := range closed {
		PublishNudgeCleared(u.String(), counts[u], ids...)
	}
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
		if closed, serr := nudgeModels.SupersedeStaleForUser(ctx, uid, nil); serr == nil {
			publishCleared(ctx, uid, closed...)
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
	memberCache := map[string]*memoryScopes{}

	var out []candidateNudge
	var stale []uuid.UUID
	defer func() {
		// Detached in one statement once the pass has looked at them all.
		if cerr := memoryModels.ClearOwners(ctx, stale); cerr != nil {
			helpers.LogErrorWithContext(ctx, "nudge engine: clear %d stale owners failed: %v", len(stale), cerr)
		}
	}()
	for _, di := range items {
		it := di.Item
		inScope, resolved := ownerScopeStatus(ctx, di.OwnerID, it, memberCache)
		if !inScope {
			// Only DETACH when we positively resolved the owner's profile and
			// confirmed they're no longer in scope. On a transient lookup
			// failure (resolved == false) we skip this pass but keep ownership
			// intact, so a Dgraph blip can't orphan items.
			if resolved {
				stale = append(stale, it.ID)
			}
			continue
		}
		// A commitment is nudged of only while it needs its person now: never
		// undated, due today or later, or stale (commitmentNeedsMeNow). It is
		// checked after the scope, so an owner who left is still detached.
		if it.Kind == memoryModels.KindCommitment && !commitmentNeedsMeNow(it, now) {
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

// ownerScopeStatus reports whether the owner currently belongs to the item's
// scope (channel / project / DM-group), and whether the owner's profile was
// resolved at all. This is the fire-time complement to memoryItemVisibleTo,
// by the same check (memoryScopes): an item is only ever nudged to an owner
// who can still see where it lives. The owner's postgres Id equals their
// Dgraph user_uuid. cache holds each owner's scopes for the pass (one lookup
// per distinct owner; nil when unresolved).
//
// Returns (inScope, resolved). When resolved is false the owner's profile
// couldn't be loaded (deleted user / transient error) and inScope is false —
// the caller must NOT clear ownership in that case, only skip the pass.
func ownerScopeStatus(ctx context.Context, ownerID uuid.UUID, it *memoryModels.MemoryItem, cache map[string]*memoryScopes) (inScope bool, resolved bool) {
	key := ownerID.String()
	scopes, ok := cache[key]
	if !ok {
		scopes = ownerScopesNow(ctx, ownerID)
		cache[key] = scopes
	}
	if scopes == nil {
		return false, false
	}
	return scopes.canRead(it), true
}

// ownerScopesNow is where a person is now, from their current Dgraph profile,
// for a reader with no request (the nudge engine, the digest). nil when the
// profile can't be loaded, so the caller fails closed rather than leaking.
func ownerScopesNow(ctx context.Context, ownerID uuid.UUID) *memoryScopes {
	info, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, ownerID.String())
	if err != nil || info == nil {
		return nil
	}
	return newMemoryScopes(info)
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

	// The work's recent dismissals in one read, and its nudges written in one
	// statement, where each was a read and a write (backend audit #36).
	keys := make([]string, 0, len(work))
	for _, c := range work {
		keys = append(keys, c.dedupKey)
	}
	handled, err := nudgeModels.RecentlyTerminal(ctx, keys, nudgeSuppressAfterTerminal)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: recent dismissals unread: %v", err)
		handled = map[string]bool{}
	}
	byKey := map[string]candidateNudge{}
	inputs := make([]nudgeModels.UpsertInput, 0, len(work))
	for _, c := range work {
		// Skip signals the user just handled (don't re-surface a dismissed one).
		if handled[c.dedupKey] {
			continue
		}
		if _, dup := byKey[c.dedupKey]; !dup {
			byKey[c.dedupKey] = c
		}
		inputs = append(inputs, nudgeModels.UpsertInput{
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
	}
	created := 0
	written, err := nudgeModels.UpsertMany(ctx, userID, inputs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: upsert failed: %v", err)
	}
	for _, u := range written {
		if u.Inserted {
			created++
			publishNudge(ctx, userID, u.ID, byKey[u.DedupKey])
		}
	}

	// Supersede the user's open nudges whose signal no longer holds (resolved /
	// no longer overdue). If the user had open nudges and none survive, this
	// clears them — keeping the bell honest.
	if closed, err := nudgeModels.SupersedeStaleForUser(ctx, userID, keep); err == nil {
		// Each one closed is told by its id, so open tabs drop exactly it.
		publishCleared(ctx, userID, closed...)
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

// publishCleared tells the person each nudge of theirs that closed, by id,
// with their open count after; nothing when none did.
func publishCleared(ctx context.Context, userID uuid.UUID, closed ...uuid.UUID) {
	if len(closed) == 0 {
		return
	}
	count, _ := nudgeModels.CountOpenForUser(ctx, userID)
	PublishNudgeCleared(userID.String(), count, closed...)
}

// ClearNudgeForMemoryItem supersedes the OPEN nudge for a memory item the
// instant its underlying signal is closed (resolved/dismissed/deleted), and
// pushes a live badge update so the bell clears immediately instead of waiting
// for the next ~30-min sweep. Best-effort: a failure just defers the clear to
// the next sweep (which supersedes stale signals anyway). Safe to call async.
func ClearNudgeForMemoryItem(ctx context.Context, memoryItemID string) {
	dedupKey := "memory:" + memoryItemID
	userID, nudgeID, err := nudgeModels.SupersedeOpenByDedupKey(ctx, dedupKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "ClearNudgeForMemoryItem supersede failed for %s: %v", memoryItemID, err)
		return
	}
	if userID == uuid.Nil {
		return // no open nudge for this item — nothing to clear
	}
	publishCleared(ctx, userID, nudgeID)
}

// PublishNudgeToUser publishes a nudge event to a single user's activity topic.
// Exported so the controller (a different package) can push live updates after
// dismiss/act actions.
func PublishNudgeToUser(userUUID string, msg *mqttStruct.MqttNudge) {
	mqttBusiness.PublishMessageToUser(userUUID, mqttStruct.MESSAGE_AI_NUDGE, msg)
}

// PublishNudgeCleared tells a person's open tabs that nudges of theirs closed:
// ONE "cleared" naming them all (nudge_ids), so each tab drops those and no
// other, with their open count after. With no ids it is the count alone (all
// of them dismissed at once: 0). The controller calls it after dismiss/act.
func PublishNudgeCleared(userUUID string, openCount int, nudgeIDs ...uuid.UUID) {
	msg := &mqttStruct.MqttNudge{Action: "cleared", OpenCount: openCount}
	for _, id := range nudgeIDs {
		msg.NudgeIDs = append(msg.NudgeIDs, id.String())
	}
	PublishNudgeToUser(userUUID, msg)
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
