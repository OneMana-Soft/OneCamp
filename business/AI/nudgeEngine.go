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
	"errors"
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

	// A signal its member dismissed or acted on stays down at least this long,
	// and after that until its item changes (nudgeHeld).
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

	owners, totalCreated := raiseNudges(ctx, time.Now())

	// Housekeeping: purge old terminal rows. Best-effort.
	if n, perr := nudgeModels.PurgeOldTerminal(ctx, nudgePurgeAfter); perr == nil && n > 0 {
		helpers.LogInfoWithContext(ctx, "nudge engine: purged %d old terminal nudge(s)", n)
	}

	helpers.LogInfoWithContext(ctx, "nudge engine: evaluated %d owner(s), created/refreshed %d nudge(s)", owners, totalCreated)
}

// raiseNudges is the pass after its gates: it raises each owner's nudges and
// closes those whose signal no longer holds, one statement per step for
// everyone (NOTIF M11: each owner was their own read, write and supersede).
// It closes only what it read as gone: a failed read raises and closes
// nothing, a read cut off at its cap raises but closes nothing, and an
// owner's items whose scope couldn't be read keep their nudges as they are.
// Before, a failed read closed every open nudge of everyone, and the next
// good pass raised them all again as new. It answers how many owners it
// evaluated and how many nudges it created.
func raiseNudges(ctx context.Context, now time.Time) (owners, created int) {
	signals, err := gatherMemoryNudges(ctx, now)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: actionable items unread, no nudge raised or closed: %v", err)
		return 0, 0
	}
	return applyNudges(ctx, now, signals)
}

// applyNudges raises and closes everyone's nudges for what a pass read
// (raiseNudges).
func applyNudges(ctx context.Context, now time.Time, signals nudgeSignals) (owners, created int) {
	// Each owner's signals, highest priority first, then stable. Their keys
	// are EVERY currently-valid signal, taken BEFORE the per-pass cap: a
	// person with more than nudgeMaxPerUser live signals never has the
	// still-valid ones superseded for missing the cap.
	byUser := map[uuid.UUID][]candidateNudge{}
	for _, c := range signals.candidates {
		byUser[c.userID] = append(byUser[c.userID], c)
	}
	signalKeys := make(map[uuid.UUID][]string, len(byUser))
	for u, list := range byUser {
		sort.SliceStable(list, func(i, j int) bool { return list[i].priority > list[j].priority })
		for _, c := range list {
			signalKeys[u] = append(signalKeys[u], c.dedupKey)
		}
	}

	// The signals their member handled stay down (nudgeHeld): when each was
	// handled is one read for everyone. Unread, nothing is raised or closed
	// this pass: a dismissal is never lost to a blip.
	handled, err := nudgeModels.LastHandled(ctx, signalKeys)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: dismissals unread, no nudge raised or closed: %v", err)
		return len(byUser), 0
	}

	// Cap how many each owner gets per pass (bounds work and nudge volume),
	// after the held ones are left out so they never take a new signal's
	// place; the rest stay in the keep-set so they're not superseded. Every
	// owner's are written in one statement.
	signal := map[string]candidateNudge{}
	var inputs []nudgeModels.UpsertInput
	for u, list := range byUser {
		n := 0
		for _, c := range list {
			if nudgeHeld(handled[u][c.dedupKey], c.changedAt, now) {
				continue
			}
			if n == nudgeMaxPerUser {
				break
			}
			n++
			if _, dup := signal[c.dedupKey]; !dup {
				signal[c.dedupKey] = c
			}
			inputs = append(inputs, nudgeModels.UpsertInput{
				UserID:     u,
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
	}
	written, err := nudgeModels.UpsertMany(ctx, inputs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: upsert failed: %v", err)
	}
	raised := map[uuid.UUID][]raisedNudge{}
	for _, w := range written {
		if w.Inserted {
			created++
			raised[w.UserID] = append(raised[w.UserID], raisedNudge{id: w.ID, signal: signal[w.DedupKey]})
		}
	}

	// Close every open nudge, of anyone, whose signal no longer holds
	// (resolved, no longer overdue, or every signal of its person resolved),
	// in one statement. Kept: each owner's signals, and the items whose
	// scope was unread. Only when the read was whole: one cut off at its cap
	// left signals unread, and no nudge is closed for a signal not read.
	var closed map[uuid.UUID][]uuid.UUID
	if signals.complete {
		keep := make(map[uuid.UUID][]string, len(signalKeys)+len(signals.held))
		for u, keys := range signalKeys {
			keep[u] = keys
		}
		for u, keys := range signals.held {
			keep[u] = slices.Concat(keep[u], keys)
		}
		if closed, err = nudgeModels.SupersedeStale(ctx, keep); err != nil {
			helpers.LogErrorWithContext(ctx, "nudge engine: supersede stale nudges failed: %v", err)
		}
	} else {
		helpers.LogErrorWithContext(ctx, "nudge engine: the actionable items read stopped at its cap (%d): no nudge closed this pass", nudgeMaxRows)
	}
	tellOwners(ctx, raised, closed)
	return len(byUser), created
}

// raisedNudge is a nudge a pass created, and the signal it was raised for.
type raisedNudge struct {
	id     uuid.UUID
	signal candidateNudge
}

// tellOwners tells each person what a pass (or one item's settle,
// ClearNudgeForMemoryItem) did to their nudges: a "new" for each one raised
// for them, and ONE "cleared" naming every one of theirs it closed (none when
// theirs only went quiet). Their open counts after are read once for
// everyone. Each event goes only to its owner's own topic.
func tellOwners(ctx context.Context, raised map[uuid.UUID][]raisedNudge, closed map[uuid.UUID][]uuid.UUID) {
	told := slices.Collect(maps.Keys(raised))
	for u := range closed {
		if _, ok := raised[u]; !ok {
			told = append(told, u)
		}
	}
	if len(told) == 0 {
		return
	}
	counts, err := nudgeModels.CountOpenForUsers(ctx, told)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "nudge engine: count the open nudges of %d person(s) failed: %v", len(told), err)
	}
	createdAt := time.Now().UTC().Format(time.RFC3339)
	for u, list := range raised {
		for _, r := range list {
			PublishNudgeToUser(u.String(), &mqttStruct.MqttNudge{
				Action:    "new",
				NudgeID:   r.id.String(),
				Kind:      r.signal.kind,
				Title:     r.signal.title,
				Body:      r.signal.body,
				CTAURL:    r.signal.ctaURL,
				CTAText:   r.signal.ctaText,
				Priority:  r.signal.priority,
				OpenCount: counts[u],
				CreatedAt: createdAt,
			})
		}
	}
	for u, ids := range closed {
		PublishNudgeCleared(u.String(), counts[u], ids...)
	}
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
	tellOwners(ctx, nil, closed)
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
	// changedAt is when the signal's item last changed: a dismissal before
	// it no longer holds the signal (nudgeHeld).
	changedAt time.Time
}

// nudgeHeld: a signal its member dismissed or acted on (at handledAt; zero
// when never) stays down for at least nudgeSuppressAfterTerminal, and after
// that until its item changes (changedAt after handledAt). The one rule for
// every nudge kind (V-615): Dismiss is an end, not a 3-day hold.
func nudgeHeld(handledAt, changedAt, now time.Time) bool {
	return !handledAt.IsZero() && (now.Sub(handledAt) < nudgeSuppressAfterTerminal || !changedAt.After(handledAt))
}

// listActionable is the pass's read of the actionable memory items: a seam,
// so a test can make the read fail.
var listActionable = memoryModels.ListActionableForDigest

// ownerProfileNow is the read of an owner's current profile (where they are):
// a seam, so a test can make the lookup fail.
var ownerProfileNow = userDomain.GetActiveDgraphUserInfoByUUID

// nudgeSignals is what a pass read: its candidate nudges; each owner's dedup
// keys of the items whose scope couldn't be read (held: their nudges stay as
// they are, neither raised nor closed); and whether the read was whole, not
// cut off at nudgeMaxRows.
type nudgeSignals struct {
	candidates []candidateNudge
	held       map[uuid.UUID][]string
	complete   bool
}

// gatherMemoryNudges turns actionable memory items (overdue commitments + stale
// open questions) into candidate nudges. This is the first rule source; more
// (blocked tasks, unreviewed PRs) can be appended here following the same shape.
// It errs when the items are unread: then the pass knows nothing.
func gatherMemoryNudges(ctx context.Context, now time.Time) (nudgeSignals, error) {
	overdueBefore := now
	staleBefore := now.Add(-nudgeStaleAfter)

	items, err := listActionable(ctx, overdueBefore, staleBefore, nudgeMaxRows)
	if err != nil {
		return nudgeSignals{}, err
	}
	out := nudgeSignals{held: map[uuid.UUID][]string{}, complete: len(items) < nudgeMaxRows}

	// PRIVACY GATE: ListActionableForDigest selects on owner_user_id only — an
	// item's owner was a scope member when it was captured, but membership
	// changes. Before pushing a nudge we re-verify the owner is STILL in the
	// item's channel/project/DM scope. Otherwise a nudge (with the item's
	// content) would follow a user out of a conversation they've left, leaking
	// it. When we can positively confirm the owner left, we also clear
	// owner_user_id so future passes don't re-scan it (self-healing).
	memberCache := map[string]ownerScope{}

	var stale []uuid.UUID
	defer func() {
		// Detached in one statement once the pass has looked at them all.
		if cerr := memoryModels.ClearOwners(ctx, stale); cerr != nil {
			helpers.LogErrorWithContext(ctx, "nudge engine: clear %d stale owners failed: %v", len(stale), cerr)
		}
	}()
	for _, di := range items {
		it := di.Item
		dedupKey := "memory:" + it.ID.String()
		inScope, resolved, serr := ownerScopeStatus(ctx, di.OwnerID, it, memberCache)
		if serr != nil {
			// Where the owner is couldn't be read: nothing is known, so the
			// item's nudge is held as it is (neither raised nor closed) and
			// it stays theirs. The lookup is one per owner, so a held owner
			// has no candidates this pass.
			out.held[di.OwnerID] = append(out.held[di.OwnerID], dedupKey)
			continue
		}
		if !inScope {
			// Only DETACH when we positively resolved the owner's profile and
			// confirmed they're no longer in scope. When their profile is gone
			// (resolved == false) we skip it but keep ownership intact.
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
		// A question likewise only while recently stale (questionNeedsMeNow).
		if it.Kind == memoryModels.KindQuestion && !questionNeedsMeNow(it, now) {
			continue
		}
		c := candidateNudge{
			userID:     di.OwnerID,
			sourceType: "memory",
			sourceID:   it.ID.String(),
			// Deep-link straight to THIS item (the panel reads ?item= to switch
			// to the open view, scroll to, and highlight it) — not the generic
			// list — so clicking the nudge lands the user on the exact thing.
			ctaURL:    "/app/ai/memory?item=" + it.ID.String(),
			ctaText:   "View",
			dedupKey:  dedupKey,
			changedAt: it.UpdatedAt,
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
		out.candidates = append(out.candidates, c)
	}
	return out, nil
}

// ownerScope is a pass's read of where an owner is: their scopes (nil when
// their profile is gone), or the error that left it unread.
type ownerScope struct {
	scopes *memoryScopes
	err    error
}

// ownerScopeStatus reports whether the owner currently belongs to the item's
// scope (channel / project / DM-group), and whether the owner's profile was
// resolved at all. This is the fire-time complement to memoryItemVisibleTo,
// by the same check (memoryScopes): an item is only ever nudged to an owner
// who can still see where it lives. The owner's postgres Id equals their
// Dgraph user_uuid. cache holds each owner's read for the pass (one lookup
// per distinct owner).
//
// Returns (inScope, resolved, err). When resolved is false the owner's
// profile is gone (deleted user) and inScope is false: the caller must NOT
// clear ownership in that case, only skip the item. err is a lookup that
// failed: nothing is known, and the caller must neither raise nor close.
func ownerScopeStatus(ctx context.Context, ownerID uuid.UUID, it *memoryModels.MemoryItem, cache map[string]ownerScope) (inScope, resolved bool, err error) {
	key := ownerID.String()
	s, ok := cache[key]
	if !ok {
		s.scopes, s.err = ownerScopesNow(ctx, ownerID)
		cache[key] = s
	}
	if s.err != nil || s.scopes == nil {
		return false, false, s.err
	}
	return s.scopes.canRead(it), true, nil
}

// ownerScopesNow is where a person is now, from their current Dgraph profile,
// for a reader with no request (the nudge engine, the digest). nil when the
// profile is gone (no live user: a deleted member), and an error when it
// couldn't be read: either way the caller fails closed rather than leaking.
// Gone is a read, not an unread one, so the pass closes a gone member's
// nudges; only an error holds them.
func ownerScopesNow(ctx context.Context, ownerID uuid.UUID) (*memoryScopes, error) {
	info, err := ownerProfileNow(ctx, ownerID.String())
	if errors.Is(err, helpers.ErrNotFound) {
		return nil, nil
	}
	if err != nil || info == nil {
		return nil, err
	}
	return newMemoryScopes(info), nil
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
	tellOwners(ctx, nil, map[uuid.UUID][]uuid.UUID{userID: {nudgeID}})
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
