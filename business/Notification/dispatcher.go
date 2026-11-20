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

	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
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
	EventProjectUpdate    EventType = "project.update"
	EventCheckIn          EventType = "checkin.ask"

	// The calendar's (calendar A2), named as business/Notify's causes:
	// invited to an event, an event changed or cancelled, and, to an
	// organiser who asked for it ("Email me when guests answer"), a guest's
	// answer. Like a booking's, on unless a person's email is off.
	EventCalendarInvited   EventType = "calendar.invited"
	EventCalendarUpdated   EventType = "calendar.updated"
	EventCalendarCancelled EventType = "calendar.cancelled"
	EventCalendarAnswered  EventType = "calendar.rsvp"
	// EventCalendarReminder is a reminder a person set to come by email
	// (calendar head A, gcal-5): sent as Requested.
	EventCalendarReminder EventType = "calendar.reminder"
	// EventCalendarRecap is the recap of an event's call, to its members
	// (gcal-new-event-notes): on unless their email_calendar_recaps is off.
	EventCalendarRecap EventType = "calendar.recap"
)

// EventTaskUrgent tells a task's assignee it was made Urgent (business/Task
// urgent.go): news of their own work, by the assigned-to-you switch.
const EventTaskUrgent EventType = "task.urgent"

// EventTaskSLA tells a task's assignee its SLA is at risk or breached
// (business/SLA alerts.go): news of their own work, by the assigned-to-you
// switch, as EventTaskUrgent.
const EventTaskSLA EventType = "task.sla"

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

	// Subject + Body are user-facing copy, as people wrote them: plain text.
	// The email shows Body as text (services/Email escapes it, once), so it
	// is never escaped or stripped before it is passed.
	SubjectLine string // becomes the email subject (e.g. "Akash mentioned you in #design")
	Title       string // headline inside the email
	Subtitle    string // smaller context line (e.g. "in #design")
	Body        string // the actual content snippet
	CTAURL      string // deep link back to the relevant view
	CTAText     string // optional override (defaults to "Open in OneCamp")

	// CTAURLFor is the deep link for one recipient, where it differs per
	// person: a DM opens at the other person, so its two sides get two
	// links. Empty, or answering "", leaves CTAURL.
	CTAURLFor func(recipientUUID string) string

	// DedupKeyParts are joined to produce the row dedup_key. Choose stable,
	// unique-per-event values: e.g. ["chat.dm", chatUUID, recipientUUID].
	// The recipient UUID is automatically appended by Dispatch.
	DedupKeyParts []string

	// Recipients is the deduplicated set of users to consider. The dispatcher
	// applies all eligibility gates per-recipient.
	Recipients []Recipient

	// About, when set, is what the email is about, kept on its queue row so
	// the worker asks again at the send whether it still holds (recheck.go).
	About *About

	// MayRead, when set, is which of the recipients (by user id, answered as
	// given) may read what the event is about now: anyone it leaves out
	// isn't emailed, and when it can't answer nobody is. An event about
	// something only some people may read sets it from that thing's read rule,
	// as a task's events do (taskReaders), so its email never reaches someone
	// the app would refuse it to, such as a commenter since taken off the
	// task's project.
	MayRead func(ctx context.Context, userIDs []string) (map[string]bool, error)

	// Hears, when set, is which of the recipients (by user id, answered as
	// given) their own setting for the place the event is in lets hear it
	// (notifylevel.Hearing, levels read once for all of them, as hears builds
	// it; no setting hears): anyone it leaves out isn't emailed, and when it can't answer nobody
	// is. A setting only says whether they are told, never what they may read.
	Hears func(ctx context.Context, userIDs []string) (map[string]bool, error)

	// SkipOnlineCheck overrides the per-user "only when offline" policy.
	// Used for events where email is the primary channel (digest, calendar
	// reminders) and where online-status is irrelevant.
	SkipOnlineCheck bool

	// Requested is an email the recipient asked for by itself, such as a
	// reminder they set to come by email. Their email switch and per-kind
	// choices don't hold it back, nor do quiet hours defer it (it is due
	// now); suppression still does. It carries no unsubscribe link, since an
	// unsubscribe never stops it: they stop it where they asked for it.
	Requested bool

	// Why is the email's footer line in place of the one about notifications:
	// for a Requested email, where to stop it.
	Why string
}

// Dispatch is the single entry point. Returns the number of rows enqueued.
// Always safe to call: where no notification email is sent (no sending key,
// essentials only, the public demo: emailService.NotificationEmailEnabled),
// returns before asking any store.
//
// Best-effort by design: per-recipient errors are logged but do not stop the
// fan-out, mirroring the FCM error handling pattern.
//
// Performance: this function used to do 4-5 PG round-trips PER recipient
// (user, suppression, prefs, online-check). We now batch the user +
// suppression + prefs lookups into 3 round-trips total regardless of
// recipient count (one more, the first time, to make the preference rows of
// those who have none), then iterate in-memory. For a 50-member channel
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
	// Step 1b: only those who may read what it is about (Event.MayRead).
	if candidates = keepBy(ctx, evt.MayRead, "who may read", evt.Type, candidates); len(candidates) == 0 {
		return 0
	}
	// Step 1c: only those whose setting for its place lets them hear it
	// (Event.Hears).
	if candidates = keepBy(ctx, evt.Hears, "whose setting lets them hear", evt.Type, candidates); len(candidates) == 0 {
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

	// Step 4: who it goes to: active, inside the workspace, with an address,
	// not one of the demo's own (helpers.IsDemoPerson: the shared visitor's
	// address is everyone who opens the demo, and the cast's and bots' take
	// no mail), not suppressed.
	to := make([]*userModels.User, 0, len(candidates))
	for _, uid := range candidates {
		user := users[uid]
		if user == nil || user.IsExternal || user.EmailID == "" || helpers.IsDemoPerson(user.EmailID) {
			continue
		}
		if reason, ok := suppressed[strings.ToLower(strings.TrimSpace(user.EmailID))]; ok && reason != "" {
			continue
		}
		to = append(to, user)
	}
	if len(to) == 0 {
		return 0
	}

	// Step 5: their preferences, in one read; the rows of those who have
	// none are made in one more statement, with the defaults (every kind on)
	// and the unsubscribe token their first email carries. A row that can't
	// be read or made sends nothing but what they asked for by itself.
	ids := make([]uuid.UUID, len(to))
	for i, user := range to {
		ids[i] = user.Id
	}
	prefs, err := prefDomain.LoadOrCreateMany(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/Dispatch prefs err: %+v", err)
	}

	// Step 6: dispatch per-recipient with all gates pre-resolved.
	enqueued := 0
	for _, user := range to {
		pref := prefs[user.Id]
		if pref == nil && evt.Requested {
			pref = &prefModels.UserNotificationPreference{}
		}
		if pref == nil {
			continue
		}
		if dispatchPrepared(ctx, evt, user, pref) {
			enqueued++
		}
	}
	return enqueued
}

// keepBy is the candidates an event's gate (Event.MayRead, Event.Hears) lets
// through, asked once for all of them: all of them when the event has no such
// gate, none when the gate can't answer. what names the gate in the log.
func keepBy(ctx context.Context, gate func(context.Context, []string) (map[string]bool, error), what string, t EventType, candidates []uuid.UUID) []uuid.UUID {
	if gate == nil || len(candidates) == 0 {
		return candidates
	}
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.String()
	}
	let, err := gate(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/Dispatch could not check %s a %s, emailing nobody: %+v", what, t, err)
		return nil
	}
	kept := make([]uuid.UUID, 0, len(candidates))
	for _, c := range candidates {
		if let[c.String()] {
			kept = append(kept, c)
		}
	}
	return kept
}

// hears is an Event.Hears gate for an event that reaches everyone it tells as
// h: levelsOf reads their settings for its place once, by the id as given.
func hears(levelsOf func(context.Context, []string) (map[string]string, error), h notifylevel.Heard) func(context.Context, []string) (map[string]bool, error) {
	return func(ctx context.Context, ids []string) (map[string]bool, error) {
		levels, err := levelsOf(ctx, ids)
		if err != nil {
			return nil, err
		}
		let := make(map[string]bool, len(ids))
		for _, id := range notifylevel.Hearing(levels, ids, func(string) notifylevel.Heard { return h }) {
			let[id] = true
		}
		return let, nil
	}
}

// enqueueEmail queues one email for the worker (a seam for tests).
var enqueueEmail = emailQueueModels.AtomicEnqueue

// dispatchPrepared is the per-recipient enqueue step run after Dispatch
// has resolved the user, suppression, and preferences in batch. The
// remaining gates (online-check, quiet-hours, render, dedup) all happen
// here.
func dispatchPrepared(ctx context.Context, evt Event, user *userModels.User, pref *prefModels.UserNotificationPreference) bool {
	if !evt.Requested && (!pref.EmailEnabled || !eventEnabledForPref(evt.Type, pref)) {
		return false
	}
	// Nothing goes out that its reader can't stop: an email they didn't ask
	// for by itself carries their row's unsubscribe link (and the
	// List-Unsubscribe header, worker.go), so one without a token is not
	// queued.
	if !evt.Requested && pref.UnsubscribeToken == "" {
		helpers.LogErrorWithContext(ctx, "notification/dispatchPrepared a %s to %s has no unsubscribe token: not queued", evt.Type, user.Id)
		return false
	}

	// Online check. Skip the email when the user has at least one
	// MQTT-connected device, unless the event opts out (digests,
	// calendar reminders).
	if !evt.SkipOnlineCheck && pref.EmailOnlyWhenOffline && isUserOnline(ctx, user.Id.String()) {
		return false
	}

	// Quiet hours and the window (dueAt). Defer by setting next_retry_at at
	// insert time so the worker can never pick the row up before it is due.
	deliverAt, deferred := dueAt(evt.Type, evt.Requested, time.Now(), pref)

	// A Requested email carries no unsubscribe link, since an unsubscribe
	// never stops it; its Why says where to stop it instead.
	unsubscribeToken := pref.UnsubscribeToken
	if evt.Requested {
		unsubscribeToken = ""
	}

	ctaURL := ctaURLFor(evt, user.Id.String())
	tmplData := emailService.TemplateData{
		RecipientName:  user.EmailID,
		ActorName:      evt.ActorName,
		ActorAvatar:    evt.ActorAvatar,
		Title:          evt.Title,
		Subtitle:       evt.Subtitle,
		Body:           evt.Body,
		CTAText:        evt.CTAText,
		CTAURL:         ctaURL,
		UnsubscribeURL: buildUnsubscribeURL(unsubscribeToken),
		BrandName:      "OneCamp",
		BrandLogo:      brandLogoURL(),
		Why:            evt.Why,
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

	meta := map[string]any{
		"event_type":        string(evt.Type),
		"actor_uuid":        evt.ActorUUID,
		"recipient":         user.Id.String(),
		"deferred":          fmt.Sprintf("%t", deferred),
		"unsubscribe_token": unsubscribeToken,
	}
	if evt.About != nil {
		meta["about"] = evt.About
	}
	if batched(evt.Type) {
		meta["line"] = noticeLine{Title: evt.Title, Place: evt.Subtitle, Snippet: trimSnippet(evt.Body, lineSnippet)}
	}
	metadata, _ := json.Marshal(meta)

	in := emailQueueModels.EnqueueInput{
		UserID:    user.Id,
		ToEmail:   user.EmailID,
		EventType: string(evt.Type),
		Subject:   subject,
		HTMLBody:  html,
		TextBody:  text,
		CTAURL:    ctaURL,
		DedupKey:  dedupKey,
		Metadata:  metadata,
	}
	if deferred {
		in.NextRetryAt = &deliverAt
	}
	inserted, err := enqueueEmail(in)
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

// ctaURLFor is the link the email to recipientUUID opens: its own
// (CTAURLFor) where the event gives one, else the event's.
func ctaURLFor(evt Event, recipientUUID string) string {
	if evt.CTAURLFor != nil {
		if u := evt.CTAURLFor(recipientUUID); u != "" {
			return u
		}
	}
	return evt.CTAURL
}

// eventEnabledForPref maps the EventType to the right toggle in the prefs.
func eventEnabledForPref(t EventType, p *prefModels.UserNotificationPreference) bool {
	switch t {
	case EventChatDM:
		return p.EmailDMs
	case EventChannelMention, EventGroupChatMention, EventCheckIn, EventDocMention, EventTableMention, EventTaskMention:
		// A check-in asks each of its people directly, as a mention does, and
		// so does an @ in a doc's body or in a comment on a table's row.
		return p.EmailMentions
	case EventTaskAssigned, EventTaskUrgent, EventTaskSLA:
		return p.EmailTaskAssigned
	case EventTaskStatus, EventProjectUpdate:
		// A project's update is news about its tasks, wanted (or not) by
		// the same people who want to hear when tasks change status.
		return p.EmailTaskStatus
	case EventTaskComment, EventPostComment, EventDocComment, EventChatComment, EventDocProposal, EventTableComment:
		// A proposed change to a doc, and what became of it, is talk about
		// the doc, wanted by whoever wants its comments.
		return p.EmailComments
	case EventChannelCall, EventChatCall:
		return p.EmailCalls
	// The calendar's emails by kind, as Google lets each person choose. A
	// booking of their time is an invitation, a cancellation a change. A
	// reminder by email (EventCalendarReminder) is the person's own ask, on
	// the event or as a default, so it has no switch here.
	case EventCalendarInvited, EventCalendarBooking:
		return p.EmailCalendarInvites
	case EventCalendarUpdated, EventCalendarCancelled:
		return p.EmailCalendarChanges
	case EventCalendarAnswered:
		return p.EmailCalendarAnswers
	case EventCalendarRecap:
		return p.EmailCalendarRecaps
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
// hours (QuietHoursUntil: the person's schedule, or the daily window) as they
// stand at that moment. Returns (whenToDeliver, deferred).
func deliveryTime(now time.Time, p *prefModels.UserNotificationPreference) (time.Time, bool) {
	from := now
	if until := p.QuietUntil(now); until != nil {
		from = *until
	}
	at, _ := p.QuietHoursUntil(from)
	return at, at.After(now)
}

// dueAt is when an email of kind t, raised at now, may go out: one asked for
// (requested) at once; a batched notice no sooner than the window's end
// (batchWindow), so what follows it goes out in the same email; and past the
// person's pause and quiet hours (deliveryTime). Returns (when, deferred). Pure.
func dueAt(t EventType, requested bool, now time.Time, p *prefModels.UserNotificationPreference) (time.Time, bool) {
	if requested {
		return now, false
	}
	from := now
	if batched(t) {
		from = now.Add(batchWindow)
	}
	// The pause and quiet hours as they stand when the window ends: quiet
	// hours starting inside it hold the notice to their end.
	at, _ := deliveryTime(from, p)
	return at, at.After(now)
}

// setNextRetryByDedup is intentionally removed — quiet-hours deferral now
// happens at insert time via EnqueueInput.NextRetryAt, eliminating the
// race where the worker could pick up a row before the deferral was applied.

// brandLogoURL returns a public URL for the configured email logo, or "".
func brandLogoURL() string {
	cfg, err := configModels.GetConfigByKey(configModels.EmailLogoKey)
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
// AppURL is a path of the app as an email links to it.
func AppURL(path string) string { return frontendBaseURL() + path }

func frontendBaseURL() string {
	return authService.FrontendBaseURL()
}

// backendBaseURL is this server's public origin. Used only for emails
// (List-Unsubscribe + email-logo URL); never for in-app navigation.
func backendBaseURL() string {
	return authService.BackendBaseURL()
}
