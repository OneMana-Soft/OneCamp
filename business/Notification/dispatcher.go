// Package notification provides the canonical fan-out point for sending
// notifications to a user across every channel: in-app activity feed,
// MQTT push, FCM web push, and email.
//
// The existing FCM/MQTT call sites in business/{Chat,Comment,Channel,Task,Post}
// continue to handle realtime delivery for active sessions. This package adds
// email as a fourth channel, gated by:
//
//  1. RESEND_API_KEY is set (else the whole subsystem is a no-op).
//  2. Recipient has email_enabled in users_notification_preferences.
//  3. Recipient has the per-event-type toggle on (email_mentions, etc.).
//  4. Recipient is not currently online on any device (configurable per-user).
//  5. Recipient's email is not in notification_email_suppressions.
//  6. Recipient is not in their own quiet-hours window (deferred to next window).
//  7. The DedupKey is not already represented by a pending/processing queue row.
//
// Email rendering happens at enqueue time so the message reflects the event
// at the moment it occurred, not at the moment the worker drains the queue.
package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	prefDomain "github.com/akashc777/OneCamp/domain/UserNotificationPreference"
	"github.com/akashc777/OneCamp/helpers"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	emailQueueModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailQueue"
	suppressionModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailSuppression"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	authService "github.com/akashc777/OneCamp/services/Auth"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

// EventType enumerates the event surface that may produce an email. Keep this
// in sync with TemplateRegistry below.
type EventType string

const (
	EventChatDM           EventType = "chat.dm"
	EventChannelMention   EventType = "channel.mention"
	EventGroupChatMention EventType = "groupchat.mention"
	EventTaskAssigned     EventType = "task.assigned"
	EventTaskStatus       EventType = "task.status_changed"
	EventTaskComment      EventType = "task.comment"
	EventPostComment      EventType = "post.comment"
	EventDocComment       EventType = "doc.comment"
	EventChatComment      EventType = "chat.comment"
	EventChannelCall      EventType = "channel.call"
	EventChatCall         EventType = "chat.call"
	EventMemoryDigest     EventType = "memory.digest"
	EventCalendarBooking  EventType = "calendar.booking"
)

// Recipient describes a single user the dispatcher should consider. Only the
// UUID is required; other fields are populated lazily.
type Recipient struct {
	UserUUID uuid.UUID
}

// Event is the canonical input to Dispatch.
type Event struct {
	Type EventType

	// ActorName / ActorAvatar describe who caused the event. ActorAvatar
	// should be a fully-qualified URL (signed MinIO URL is fine).
	ActorName   string
	ActorAvatar string
	ActorUUID   string

	// Subject + Body are user-facing copy. Body is treated as plain text
	// inside <span>s in the rendered template — pre-strip HTML before passing.
	SubjectLine string // becomes the email subject (e.g. "Akash mentioned you in #design")
	Title       string // headline inside the email
	Subtitle    string // smaller context line (e.g. "in #design")
	Body        string // the actual content snippet
	CTAURL      string // deep link back to the relevant view
	CTAText     string // optional override (defaults to "Open in OneCamp")

	// DedupKeyParts are joined to produce the row dedup_key. Choose stable,
	// unique-per-event values: e.g. ["chat.dm", chatUUID, recipientUUID].
	// The recipient UUID is automatically appended by Dispatch.
	DedupKeyParts []string

	// Recipients is the deduplicated set of users to consider. The dispatcher
	// applies all eligibility gates per-recipient.
	Recipients []Recipient

	// SkipOnlineCheck overrides the per-user "only when offline" policy.
	// Used for events where email is the primary channel (digest, calendar
	// reminders) and where online-status is irrelevant.
	SkipOnlineCheck bool
}

// Dispatch is the single entry point. Returns the number of rows enqueued.
// Always safe to call: when RESEND_API_KEY is unset, returns immediately.
//
// Best-effort by design: per-recipient errors are logged but do not stop the
// fan-out, mirroring the FCM error handling pattern.
//
// Performance: this function used to do 4-5 PG round-trips PER recipient
// (user, suppression, prefs, online-check). We now batch the user +
// suppression + prefs lookups into 3 round-trips total regardless of
// recipient count, then iterate in-memory. For a 50-member channel
// mention this drops ~250 PG calls to 3.
func Dispatch(ctx context.Context, evt Event) int {
	if !emailService.NotificationEmailEnabled() {
		return 0
	}
	if len(evt.Recipients) == 0 {
		return 0
	}
	if evt.Type == "" {
		helpers.LogErrorWithContext(ctx, "notification/Dispatch missing event type")
		return 0
	}

	// Step 1: deduplicate + filter self-notifications.
	candidates := make([]uuid.UUID, 0, len(evt.Recipients))
	seen := make(map[uuid.UUID]struct{}, len(evt.Recipients))
	for _, r := range evt.Recipients {
		if r.UserUUID == uuid.Nil {
			continue
		}
		if evt.ActorUUID != "" && r.UserUUID.String() == evt.ActorUUID {
			continue
		}
		if _, dup := seen[r.UserUUID]; dup {
			continue
		}
		seen[r.UserUUID] = struct{}{}
		candidates = append(candidates, r.UserUUID)
	}
	if len(candidates) == 0 {
		return 0
	}

	// Step 2: batch-load active users (filters out deleted + external).
	users, err := userDomain.GetActiveUsersByUUIDsForNotification(ctx, candidates)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/Dispatch batch user load err: %+v", err)
		return 0
	}

	// Step 3: collect emails of non-external recipients for the
	// suppression batch lookup.
	emails := make([]string, 0, len(users))
	for _, u := range users {
		if u == nil || u.IsExternal || u.EmailID == "" {
			continue
		}
		emails = append(emails, u.EmailID)
	}

	suppressed, err := suppressionModels.IsSuppressedBatch(emails)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/Dispatch batch suppression err: %+v", err)
		// Fail open — better to over-email than under-email when the
		// suppression list is briefly unreachable.
		suppressed = map[string]string{}
	}

	// Step 4: batch-load preferences. We don't auto-create rows here
	// — a missing row means "use defaults" and we can compute that
	// in-memory.
	prefs, err := prefDomain.LoadByUserIDs(ctx, candidates)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/Dispatch batch prefs err: %+v", err)
		// Fail open same as above.
		prefs = map[uuid.UUID]*prefModels.UserNotificationPreference{}
	}

	// Step 5: dispatch per-recipient with all gates pre-resolved.
	enqueued := 0
	for _, uid := range candidates {
		user := users[uid]
		if user == nil {
			continue
		}
		if user.IsExternal || user.EmailID == "" {
			continue
		}
		if reason, ok := suppressed[strings.ToLower(strings.TrimSpace(user.EmailID))]; ok && reason != "" {
			continue
		}
		pref := prefs[uid]
		if pref == nil {
			// No preference row exists. We *could* lazily create one
			// here, but it's cheaper to delay creation to the first
			// time the user opens the settings page. Treat missing as
			// "email disabled" — the user has not opted into emails.
			continue
		}
		if dispatchPrepared(ctx, evt, user, pref) {
			enqueued++
		}
	}
	return enqueued
}

// dispatchPrepared is the per-recipient enqueue step run after Dispatch
// has resolved the user, suppression, and preferences in batch. The
// remaining gates (online-check, quiet-hours, render, dedup) all happen
// here.
func dispatchPrepared(ctx context.Context, evt Event, user *userModels.User, pref *prefModels.UserNotificationPreference) bool {
	if !pref.EmailEnabled {
		return false
	}
	if !eventEnabledForPref(evt.Type, pref) {
		return false
	}

	// Online check. Skip the email when the user has at least one
	// MQTT-connected device, unless the event opts out (digests,
	// calendar reminders).
	if !evt.SkipOnlineCheck && pref.EmailOnlyWhenOffline && isUserOnline(ctx, user.Id.String()) {
		return false
	}

	// Quiet hours. Defer to the next post-quiet-hours moment by setting
	// next_retry_at at insert time so the worker can never pick the row
	// up before the window closes.
	deliverAt, deferred := deliveryTime(time.Now(), pref)

	tmplData := emailService.TemplateData{
		RecipientName:  user.EmailID,
		ActorName:      evt.ActorName,
		ActorAvatar:    evt.ActorAvatar,
		Title:          evt.Title,
		Subtitle:       evt.Subtitle,
		Body:           evt.Body,
		CTAText:        evt.CTAText,
		CTAURL:         evt.CTAURL,
		UnsubscribeURL: buildUnsubscribeURL(pref.UnsubscribeToken),
		BrandName:      "OneCamp",
		BrandLogo:      brandLogoURL(),
	}
	html, text, err := emailService.Render(tmplData)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"notification/dispatchPrepared render failed event=%s err=%+v", evt.Type, err)
		return false
	}

	subject := evt.SubjectLine
	if subject == "" {
		subject = evt.Title
	}
	if subject == "" {
		subject = "New notification on OneCamp"
	}

	dedupParts := append([]string{string(evt.Type)}, evt.DedupKeyParts...)
	dedupParts = append(dedupParts, user.Id.String())
	dedupKey := strings.Join(dedupParts, "|")
	if len(dedupKey) > 480 {
		dedupKey = dedupKey[:240] + "|" + helpers.HashString(dedupKey)
	}

	metadata, _ := json.Marshal(map[string]string{
		"event_type":        string(evt.Type),
		"actor_uuid":        evt.ActorUUID,
		"recipient":         user.Id.String(),
		"deferred":          fmt.Sprintf("%t", deferred),
		"unsubscribe_token": pref.UnsubscribeToken,
	})

	in := emailQueueModels.EnqueueInput{
		UserID:    user.Id,
		ToEmail:   user.EmailID,
		EventType: string(evt.Type),
		Subject:   subject,
		HTMLBody:  html,
		TextBody:  text,
		CTAURL:    evt.CTAURL,
		DedupKey:  dedupKey,
		Metadata:  metadata,
	}
	if deferred {
		in.NextRetryAt = &deliverAt
	}
	inserted, err := emailQueueModels.AtomicEnqueue(in)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"notification/dispatchPrepared enqueue failed event=%s err=%+v", evt.Type, err)
		return false
	}
	if !inserted {
		return false
	}

	signalEmailWorker()
	return true
}

// eventEnabledForPref maps the EventType to the right toggle in the prefs.
func eventEnabledForPref(t EventType, p *prefModels.UserNotificationPreference) bool {
	switch t {
	case EventChatDM:
		return p.EmailDMs
	case EventChannelMention, EventGroupChatMention:
		return p.EmailMentions
	case EventTaskAssigned:
		return p.EmailTaskAssigned
	case EventTaskStatus:
		return p.EmailTaskStatus
	case EventTaskComment, EventPostComment, EventDocComment, EventChatComment:
		return p.EmailComments
	case EventChannelCall, EventChatCall:
		return p.EmailCalls
	default:
		return true
	}
}

// isUserOnline returns true when the user has at least one MQTT-connected
// device, per the user_device_connected counter we already increment on
// MQTT connect. Best-effort: errors fall through to "offline" so the email
// goes out; it's better to over-email than to silently drop a notification.
func isUserOnline(ctx context.Context, userUUID string) bool {
	dgraphUser, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil || dgraphUser == nil {
		return false
	}
	if dgraphUser.DevicesConnected == nil {
		return false
	}
	return *dgraphUser.DevicesConnected > 0
}

// deliveryTime is when an email may go out: after a pause, then past quiet
// hours as they stand at that moment. Returns (whenToDeliver, deferred).
func deliveryTime(now time.Time, p *prefModels.UserNotificationPreference) (time.Time, bool) {
	from := now
	if p.NotificationsPausedUntil != nil && p.NotificationsPausedUntil.After(now) {
		from = *p.NotificationsPausedUntil
	}
	at, _ := quietHoursDelay(from, p)
	return at, at.After(now)
}

// quietHoursDelay decides whether we should defer the email past the user's
// quiet-hours window. Returns (whenToDeliver, deferred).
func quietHoursDelay(now time.Time, p *prefModels.UserNotificationPreference) (time.Time, bool) {
	if !p.QuietHoursEnabled || p.QuietHoursStart == nil || p.QuietHoursEnd == nil {
		return now, false
	}
	loc := time.UTC
	if p.QuietHoursTZ != nil && *p.QuietHoursTZ != "" {
		if l, err := time.LoadLocation(*p.QuietHoursTZ); err == nil {
			loc = l
		}
	}
	local := now.In(loc)

	startH, startM, ok1 := parseHHMM(*p.QuietHoursStart)
	endH, endM, ok2 := parseHHMM(*p.QuietHoursEnd)
	if !ok1 || !ok2 {
		return now, false
	}

	startToday := time.Date(local.Year(), local.Month(), local.Day(), startH, startM, 0, 0, loc)
	endToday := time.Date(local.Year(), local.Month(), local.Day(), endH, endM, 0, 0, loc)

	// Two cases: same-day window (08:00 → 17:00) and overnight window (22:00 → 07:00).
	// A window that starts where it ends is empty, as the push gate reads it.
	if endToday.Equal(startToday) {
		return now, false
	}
	if endToday.After(startToday) {
		// Same-day. If now is inside, deliver at end.
		if (local.Equal(startToday) || local.After(startToday)) && local.Before(endToday) {
			return endToday, true
		}
		return now, false
	}
	// Overnight: in window if now ≥ start OR now < end (next morning).
	if local.Equal(startToday) || local.After(startToday) {
		// In window, deliver at end (next day).
		return endToday.AddDate(0, 0, 1), true
	}
	if local.Before(endToday) {
		return endToday, true
	}
	return now, false
}

func parseHHMM(s string) (int, int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// setNextRetryByDedup is intentionally removed — quiet-hours deferral now
// happens at insert time via EnqueueInput.NextRetryAt, eliminating the
// race where the worker could pick up a row before the deferral was applied.

// brandLogoURL returns a public URL for the configured email logo, or "".
func brandLogoURL() string {
	cfg, err := configModels.GetConfigByKey("invitation_email_logo")
	if err != nil || cfg == nil || cfg.Value == "" {
		return ""
	}
	return backendBaseURL() + "/public/email/logo"
}

// buildUnsubscribeURL returns the canonical one-click unsubscribe URL.
//
// We point at the BACKEND, not the FE, for two reasons:
//  1. RFC 8058 List-Unsubscribe-Post one-click does an HTTP POST to this
//     URL. Mailbox providers (Gmail, Outlook, etc.) issue that POST
//     directly — it has to land on a real API endpoint, not a Next.js page.
//  2. The same URL works for a human clicking the footer link via GET.
//     The BE handler responds 200 either way and (optionally) redirects
//     humans to the FE confirmation page.
func buildUnsubscribeURL(token string) string {
	if token == "" {
		return ""
	}
	return backendBaseURL() + "/public/notifications/unsubscribe?token=" + token
}

// frontendBaseURL is the package-private accessor used by wiring.go and
// the dispatcher. It localises the import on services/Auth so callers in
// this package can use frontendBaseURL() directly.
func frontendBaseURL() string {
	return authService.FrontendBaseURL()
}

// backendBaseURL is this server's public origin. Used only for emails
// (List-Unsubscribe + email-logo URL); never for in-app navigation.
func backendBaseURL() string {
	return authService.BackendBaseURL()
}
