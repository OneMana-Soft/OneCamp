package business

// briefingConnectors.go — the cross-connector "Your day" section of the
// personal briefing. This is the daily-habit payoff of the connector layer:
// the moment a user opens OneCamp, the briefing answers "what's my day" across
// every tool they've linked — today's meetings, pull requests waiting on them,
// and important unread email — alongside their workspace open items.
//
// Production properties (mirrors briefing.go):
//   - Each connector is queried in PARALLEL under one shared, tight deadline,
//     so a slow external API can never block the home screen.
//   - Per-user only: every call resolves the connector from the requesting
//     user's UUID inside the connector layer.
//   - Degrades gracefully: a connector that's unlinked, errors, or times out
//     simply contributes nothing — the section renders whatever it gathered.
//   - Cheap when nothing is connected: each helper returns immediately on the
//     "not connected" sentinel without spending the budget.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

const (
	// connectorBriefingTimeout bounds the whole cross-connector fan-out. It's
	// deliberately a touch longer than briefingTimeout because these are
	// external network calls, but still well under a second-and-a-half so the
	// home screen stays snappy.
	connectorBriefingTimeout = 2500 * time.Millisecond

	briefingMaxCalendar = 4
	briefingMaxPRs      = 5
	briefingMaxEmails   = 5
)

// gatherConnectorDay assembles the merged "Your day" agenda for a user across
// their linked connectors. Cached per-user for a few minutes so repeated home
// loads don't re-hit external APIs. Never errors — returns whatever it could
// gather.
func gatherConnectorDay(ctx context.Context, userInfo *userModels.UserInfo) []adapter.BriefingDayItem {
	uid := userInfo.UserPostgresInfo.Id

	// Serve from cache when warm.
	cacheArgs := []string{uid.String()}
	var cached []adapter.BriefingDayItem
	if found, _ := redisStore.GetJSON(ctx, registry.ConnectorBriefingDay, cacheArgs, &cached); found {
		return cached
	}

	day := computeConnectorDay(ctx, userInfo)

	// Cache even an empty result so a user with no connectors (or a quiet day)
	// doesn't trigger the IsConnected probes + external calls every load.
	_ = redisStore.SetJSON(ctx, registry.ConnectorBriefingDay, cacheArgs, day)
	return day
}

// computeConnectorDay does the actual cross-connector fan-out.
func computeConnectorDay(ctx context.Context, userInfo *userModels.UserInfo) []adapter.BriefingDayItem {
	uid := userInfo.UserPostgresInfo.Id

	// Fast exit: if the user has linked nothing, skip the whole fan-out.
	gmailOn := connectorBusiness.IsConnected(ctx, uid, connectorBusiness.ProviderGmail)
	calOn := connectorBusiness.IsConnected(ctx, uid, connectorBusiness.ProviderCalendar)
	ghOn := connectorBusiness.IsConnected(ctx, uid, connectorBusiness.ProviderGitHub)
	if !gmailOn && !calOn && !ghOn {
		return nil
	}

	bctx, cancel := context.WithTimeout(ctx, connectorBriefingTimeout)
	defer cancel()

	type partial struct {
		items []adapter.BriefingDayItem
	}
	results := make(chan partial, 3)
	pending := 0

	if calOn {
		pending++
		helpers.GoSafeSend("briefing.calendar", results, func() partial {
			return partial{items: briefingCalendar(bctx, uid)}
		})
	}
	if ghOn {
		pending++
		helpers.GoSafeSend("briefing.github", results, func() partial {
			return partial{items: briefingGitHub(bctx, uid)}
		})
	}
	if gmailOn {
		pending++
		helpers.GoSafeSend("briefing.gmail", results, func() partial {
			return partial{items: briefingGmail(bctx, uid)}
		})
	}

	var day []adapter.BriefingDayItem
	for i := 0; i < pending; i++ {
		select {
		case p := <-results:
			day = append(day, p.items...)
		case <-bctx.Done():
			i = pending // budget exhausted; ship what we have
		}
	}

	// Order: today's calendar first (time-sensitive), then PRs/issues, then
	// email. Stable so equal-source items keep their provider order.
	sourceRank := map[string]int{"calendar": 0, "github": 1, "gmail": 2}
	sort.SliceStable(day, func(i, j int) bool {
		return sourceRank[day[i].Source] < sourceRank[day[j].Source]
	})
	return day
}

func briefingCalendar(ctx context.Context, uid uuid.UUID) []adapter.BriefingDayItem {
	events, err := connectorBusiness.CalendarList(ctx, uid, 1, briefingMaxCalendar)
	if err != nil {
		logConnectorBriefingErr(ctx, "calendar", err)
		return nil
	}
	out := make([]adapter.BriefingDayItem, 0, len(events))
	for _, e := range events {
		out = append(out, adapter.BriefingDayItem{
			Source:   "calendar",
			Kind:     "event",
			Title:    strings.TrimSpace(e.Title),
			Subtitle: friendlyEventTime(e.Start),
			URL:      e.Link,
		})
	}
	return out
}

func briefingGitHub(ctx context.Context, uid uuid.UUID) []adapter.BriefingDayItem {
	prs, err := connectorBusiness.GitHubListMyPRs(ctx, uid, briefingMaxPRs)
	if err != nil {
		logConnectorBriefingErr(ctx, "github", err)
		return nil
	}
	out := make([]adapter.BriefingDayItem, 0, len(prs))
	for _, p := range prs {
		repo := strings.TrimPrefix(p.Repo, "/")
		out = append(out, adapter.BriefingDayItem{
			Source:   "github",
			Kind:     "pr",
			Title:    strings.TrimSpace(p.Title),
			Subtitle: fmt.Sprintf("%s#%d", repo, p.Number),
			URL:      p.URL,
		})
	}
	return out
}

func briefingGmail(ctx context.Context, uid uuid.UUID) []adapter.BriefingDayItem {
	// High-signal morning email: unread, in inbox, last 3 days, explicitly
	// NOT in promotions/social/updates/forums/spam. We use 3 days so a
	// quieter inbox still shows real mail. We fetch 4× the display cap and
	// filter in-process so promotional senders that Gmail misplaces in Primary
	// are removed before the user sees them.
	const query = "in:inbox is:unread newer_than:3d -category:promotions -category:social -category:updates -category:forums -category:spam"
	// Fetch enough headroom to survive the sender filter.
	emails, err := connectorBusiness.GmailSearch(ctx, uid, query, int64(briefingMaxEmails*4))
	if err != nil {
		logConnectorBriefingErr(ctx, "gmail", err)
		return nil
	}

	out := make([]adapter.BriefingDayItem, 0, briefingMaxEmails)
	for _, m := range emails {
		if len(out) >= briefingMaxEmails {
			break
		}
		// Skip automated/no-reply senders — they are marketing even when
		// Gmail puts them in Primary. A real person email never comes from
		// no-reply@, noreply@, donotreply@, or a .notifications.com domain.
		if isPromotionalSender(m.From) {
			continue
		}
		subject := strings.TrimSpace(m.Subject)
		if subject == "" {
			subject = "(no subject)"
		}
		out = append(out, adapter.BriefingDayItem{
			Source:   "gmail",
			Kind:     "email",
			Title:    subject,
			Subtitle: senderName(m.From),
			URL:      gmailMessageURL(m.ID),
		})
	}
	return out
}

// isPromotionalSender reports whether a From address looks like an automated
// sender (marketing, notifications, no-reply) rather than a real person.
// We keep the keyword list narrow and conservative: only match local-parts
// that are unambiguously bulk/automated. Broad keywords like "info", "hello",
// "team", "support" are intentionally excluded — they produce too many false
// positives on legitimate business correspondence.
func isPromotionalSender(from string) bool {
	addr := strings.ToLower(from)

	// Extract the email address between < > if present.
	email := addr
	if start := strings.LastIndex(addr, "<"); start >= 0 {
		if end := strings.Index(addr[start:], ">"); end >= 0 {
			email = strings.TrimSpace(addr[start+1 : start+end])
		}
	}

	// Get the local part (before @).
	local := email
	if at := strings.Index(email, "@"); at >= 0 {
		local = email[:at]
	}

	// Get the domain part (after @).
	domain := ""
	if at := strings.LastIndex(email, "@"); at >= 0 {
		domain = email[at+1:]
	}

	// Unambiguous bulk/automated local-part keywords.
	bulkLocals := []string{
		"noreply", "no-reply", "no.reply",
		"donotreply", "do-not-reply", "do.not.reply",
		"notifications", "notification",
		"newsletter", "newsletters",
		"mailer", "mailbot",
		"postmaster", "bounce", "bounces",
		"robot", "automated", "automailer",
	}
	for _, kw := range bulkLocals {
		if local == kw || strings.HasPrefix(local, kw+"-") || strings.HasPrefix(local, kw+".") || strings.HasPrefix(local, kw+"_") {
			return true
		}
	}

	// Marketing/notification subdomain patterns in the domain.
	notifSubdomains := []string{
		"notifications.", "notify.", "marketing.",
		"campaigns.", "promo.", "bulk.", "em.", "em2.",
	}
	for _, sub := range notifSubdomains {
		if strings.HasPrefix(domain, sub) {
			return true
		}
	}

	return false
}

// gmailMessageURL builds a deep link that opens a specific Gmail message. The
// Gmail web client opens a message by its API id via the #all/<id> fragment
// (works regardless of which label/tab the message lives in). Falls back to the
// inbox when no id is available rather than a broken search URL.
func gmailMessageURL(messageID string) string {
	id := strings.TrimSpace(messageID)
	if id == "" {
		return "https://mail.google.com/mail/u/0/#inbox"
	}
	return "https://mail.google.com/mail/u/0/#all/" + id
}

// logConnectorBriefingErr logs at info level for the expected "not connected"
// sentinel (shouldn't happen given the IsConnected gate, but be safe) and at
// error level for genuine failures. Never propagates — the briefing degrades.
func logConnectorBriefingErr(ctx context.Context, source string, err error) {
	if errors.Is(err, connectorBusiness.ErrNotConnected) {
		return
	}
	helpers.LogInfoWithContext(ctx, "briefing connector %s failed: %v", source, err)
}

// friendlyEventTime renders an RFC3339 (or date-only) start into a compact
// "3:04 PM" / "All day" label for the agenda row.
func friendlyEventTime(start string) string {
	if start == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, start); err == nil {
		return t.Local().Format("Mon 3:04 PM")
	}
	// Date-only (all-day event).
	if _, err := time.Parse("2006-01-02", start); err == nil {
		return "All day"
	}
	return start
}

// senderName extracts a display name from an RFC5322 From header
// ("Sarah <sarah@x.com>" → "Sarah"), falling back to the address.
func senderName(from string) string {
	from = strings.TrimSpace(from)
	if i := strings.Index(from, "<"); i > 0 {
		name := strings.TrimSpace(from[:i])
		name = strings.Trim(name, "\"")
		if name != "" {
			return name
		}
	}
	return strings.Trim(from, "<>")
}
