package notification

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// recoverDispatch is the panic guard wrapped around every fire-and-forget
// goroutine in this file. We log the panic and stack so a buggy callsite
// produces a useful trace instead of vanishing silently. Returning here
// is intentional: a panic in one recipient must not poison the others.
func recoverDispatch(label string) {
	if r := recover(); r != nil {
		helpers.LogErrorWithContext(context.Background(),
			"notification/%s panic recovered: %v", label, r)
	}
}

// This file is the public surface used by other business packages to
// dispatch email notifications without rebuilding the Event struct themselves.
// Each helper maps directly to one of the FCM emit sites we already have, so
// a new event type means one new helper here and one extra `go notification.X(...)`
// at the existing FCM call site — no edits to recipient lookup, online-check,
// or template logic.

// trimSnippet trims a body to a sane preview length without breaking inside
// a UTF-8 rune. The dispatcher template re-renders without HTML tags for
// the plain-text alternative, so trimming on the rune boundary is enough.
func trimSnippet(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Trim down to max bytes but back up to the previous rune boundary.
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// frontendChatURL builds the deep link for a DM/group conversation.
func frontendChatURL(grpId string) string {
	return frontendBaseURL() + "/app/chat/" + grpId
}

// frontendChannelPostURL deep-links to a specific post in a channel.
func frontendChannelPostURL(channelUUID, postUUID string) string {
	return frontendBaseURL() + "/app/channel/" + channelUUID + "/" + postUUID
}

// frontendChannelURL deep-links to a channel root.
func frontendChannelURL(channelUUID string) string {
	return frontendBaseURL() + "/app/channel/" + channelUUID
}

// frontendTaskURL deep-links to a task.
func frontendTaskURL(taskUUID string) string {
	return frontendBaseURL() + "/app/task/" + taskUUID
}

// frontendDocURL deep-links to a doc.
func frontendDocURL(docUUID string) string {
	return frontendBaseURL() + "/app/doc/" + docUUID
}

// frontendMemoryURL deep-links to the workspace-memory surface.
func frontendMemoryURL() string {
	return frontendBaseURL() + "/app/ai/memory"
}

// DispatchMemoryDigest sends a single recipient their personal "open items"
// digest (overdue commitments + stale open questions the AI captured). This
// is the proactive arm of the memory layer: instead of waiting to be asked
// "what's still open", OneCamp surfaces it. Email is the PRIMARY channel
// here, so the online-check is skipped. `body` is a plain-text summary
// (the dispatcher template renders Body as text, not HTML). Dedup is per
// (recipient, date) so a user gets at most one digest per run-day even if
// the worker is retried.
func DispatchMemoryDigest(recipientUUID, subject, body string) {
	if recipientUUID == "" || body == "" {
		return
	}
	go func() {
		defer recoverDispatch("DispatchMemoryDigest")
		ctx := context.Background()
		Dispatch(ctx, Event{
			Type:            EventMemoryDigest,
			SubjectLine:     subject,
			Title:           "Your open items",
			Subtitle:        "From your workspace memory",
			Body:            body,
			CTAURL:          frontendMemoryURL(),
			CTAText:         "Review in OneCamp",
			DedupKeyParts:   []string{"memory_digest", helpers.NowDateKey()},
			Recipients:      recipientsFromStrings([]string{recipientUUID}),
			SkipOnlineCheck: true,
		})
	}()
}

// DispatchCalendarBooking tells a booking page's owner that someone booked
// (or cancelled) time with them. Email is the channel: a booking lands while
// the owner is away from OneCamp more often than not.
func DispatchCalendarBooking(ownerUUID, guestName, pageTitle, when, bookingID string, cancelled bool) {
	if ownerUUID == "" {
		return
	}
	go func() {
		defer recoverDispatch("DispatchCalendarBooking")
		subject, title, kind := guestName+" booked "+pageTitle, "New booking", "booked"
		if cancelled {
			subject, title, kind = guestName+" cancelled "+pageTitle, "Booking cancelled", "cancelled"
		}
		Dispatch(context.Background(), Event{
			Type:            EventCalendarBooking,
			ActorName:       guestName,
			SubjectLine:     subject,
			Title:           title,
			Subtitle:        pageTitle,
			Body:            when,
			CTAURL:          frontendBaseURL() + "/app/calendar",
			CTAText:         "Open your calendar",
			DedupKeyParts:   []string{"calendar.booking", bookingID, kind},
			Recipients:      recipientsFromStrings([]string{ownerUUID}),
			SkipOnlineCheck: true,
		})
	}()
}

// DispatchProjectUpdate tells a project's members that one of its admins
// posted an update: where the project stands, and the note. The author is
// left out.
func DispatchProjectUpdate(authorUUID, authorName, projectUUID, projectName, healthLabel, body, updateID string, members []string) {
	recipients := make([]string, 0, len(members))
	for _, m := range members {
		if m != "" && m != authorUUID {
			recipients = append(recipients, m)
		}
	}
	if len(recipients) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchProjectUpdate")
		Dispatch(context.Background(), Event{
			Type:          EventProjectUpdate,
			ActorName:     authorName,
			ActorUUID:     authorUUID,
			SubjectLine:   projectName + ": " + healthLabel,
			Title:         authorName + " posted an update",
			Subtitle:      projectName + " · " + healthLabel,
			Body:          body,
			CTAURL:        frontendBaseURL() + "/app/project/" + projectUUID + "?tab=updates",
			CTAText:       "Read the update",
			DedupKeyParts: []string{"project.update", updateID},
			Recipients:    recipientsFromStrings(recipients),
		})
	}()
}

// DispatchMemoryDigestTest sends a one-off TEST digest to a single recipient,
// bypassing the per-(recipient, day) dedup so an admin can re-send and verify
// delivery. It still honors the recipient's email settings + suppression
// downstream in Dispatch. The unique dedup token (nanosecond) guarantees each
// test is delivered rather than collapsed into the daily digest.
// DispatchMemoryDigestTestNow is the SYNCHRONOUS variant, for the admin
// "send me a test digest" button. It returns how many recipients were actually
// dispatched to.
//
// The scheduled digest dispatches fire-and-forget, which is right when nobody is
// waiting on the result. It is wrong for a test, because the entire
// point of the button is to find out whether delivery works, and an async
// dispatch reports success before it knows. That is how a PREPARE-time SQL
// failure in the email queue went unnoticed: the button said "Test digest sent"
// every time while nothing was ever enqueued.
func DispatchMemoryDigestTestNow(ctx context.Context, recipientUUID, subject, body string) int {
	if recipientUUID == "" || body == "" {
		return 0
	}
	defer recoverDispatch("DispatchMemoryDigestTestNow")
	return Dispatch(ctx, Event{
		Type:            EventMemoryDigest,
		SubjectLine:     subject,
		Title:           "Your open items (test)",
		Subtitle:        "From your workspace memory",
		Body:            body,
		CTAURL:          frontendMemoryURL(),
		CTAText:         "Review in OneCamp",
		DedupKeyParts:   []string{"memory_digest_test", strconv.FormatInt(time.Now().UnixNano(), 10)},
		Recipients:      recipientsFromStrings([]string{recipientUUID}),
		SkipOnlineCheck: true,
	})
}

// DispatchChatDM is the integration point used by business/Chat right after
// the FCM multicast for a 1:1 / group DM message.
//
// Recipients are the eligible-for-notifications user UUIDs already computed
// for FCM. The actor is the sender. mentioned is optional and tags the
// "mentioned in DM" flavour, but for direct DMs the mention list is
// usually empty — the message itself is the notification.
func DispatchChatDM(senderUUID, senderName string, senderAvatar string,
	grpId, chatUUID, plainTextBody string,
	recipientUUIDs []string, mentioned bool) {

	if len(recipientUUIDs) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchChatDM")
		ctx := context.Background()
		title := senderName + " sent you a message"
		if mentioned {
			title = senderName + " mentioned you"
		}
		Dispatch(ctx, Event{
			Type:          EventChatDM,
			ActorUUID:     senderUUID,
			ActorName:     senderName,
			ActorAvatar:   senderAvatar,
			SubjectLine:   title,
			Title:         title,
			Subtitle:      "Direct message",
			Body:          trimSnippet(plainTextBody, 600),
			CTAURL:        frontendChatURL(grpId),
			CTAText:       "View conversation",
			DedupKeyParts: []string{grpId, chatUUID},
			Recipients:    recipientsFromStrings(recipientUUIDs),
		})
	}()
}

// DispatchChannelMention is the integration point for channel post mentions.
// Only the explicit mentionUUIDs receive an email — channel posts in general
// are too noisy to email everyone. Use this for both new posts AND post edits
// where new mentions were introduced.
func DispatchChannelMention(actorUUID, actorName, actorAvatar string,
	channelUUID, channelName, postUUID, postTextPlain string,
	mentionUUIDs []string) {

	if len(mentionUUIDs) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchChannelMention")
		ctx := context.Background()
		subject := actorName + " mentioned you in #" + channelName
		Dispatch(ctx, Event{
			Type:          EventChannelMention,
			ActorUUID:     actorUUID,
			ActorName:     actorName,
			ActorAvatar:   actorAvatar,
			SubjectLine:   subject,
			Title:         subject,
			Subtitle:      "in #" + channelName,
			Body:          trimSnippet(postTextPlain, 600),
			CTAURL:        frontendChannelPostURL(channelUUID, postUUID),
			CTAText:       "View post",
			DedupKeyParts: []string{channelUUID, postUUID},
			Recipients:    recipientsFromStrings(mentionUUIDs),
		})
	}()
}

// DispatchTaskAssignment is the integration point for task assignee changes.
// Triggered every time UpdateTaskAssignee assigns a task to someone other
// than the actor.
func DispatchTaskAssignment(actorUUID, actorName, actorAvatar string,
	taskUUID, taskName, projectName string,
	assigneeUUID string) {

	if assigneeUUID == "" || assigneeUUID == actorUUID {
		return
	}
	go func() {
		defer recoverDispatch("DispatchTaskAssignment")
		ctx := context.Background()
		subject := actorName + " assigned a task to you"
		body := taskName
		if projectName != "" {
			body = projectName + " · " + taskName
		}
		Dispatch(ctx, Event{
			Type:          EventTaskAssigned,
			ActorUUID:     actorUUID,
			ActorName:     actorName,
			ActorAvatar:   actorAvatar,
			SubjectLine:   subject,
			Title:         subject,
			Subtitle:      "Task assigned",
			Body:          trimSnippet(body, 300),
			CTAURL:        frontendTaskURL(taskUUID),
			CTAText:       "Open task",
			DedupKeyParts: []string{"assigned", taskUUID, assigneeUUID},
			Recipients:    recipientsFromStrings([]string{assigneeUUID}),
		})
	}()
}

// DispatchTaskStatusChange tells the people a task belongs to (its assignee,
// and its creator when known) that someone else moved it: "Maya moved Set up
// SSO to QA". Statuses are named as people see them, a project's own by its
// name. The actor is never told of their own change (Dispatch drops them).
func DispatchTaskStatusChange(actorUUID, actorName, actorAvatar string,
	taskUUID, taskName, projectName, fromStatus, toStatus, changeID string,
	recipientUUIDs []string) {

	if len(recipientUUIDs) == 0 || taskUUID == "" {
		return
	}
	go func() {
		defer recoverDispatch("DispatchTaskStatusChange")
		ctx := context.Background()
		name := taskName
		if name == "" {
			name = "a task"
		}
		subject := actorName + " moved " + name + " to " + toStatus
		body := fromStatus + " → " + toStatus
		if projectName != "" {
			body = projectName + " · " + body
		}
		Dispatch(ctx, Event{
			Type:          EventTaskStatus,
			ActorUUID:     actorUUID,
			ActorName:     actorName,
			ActorAvatar:   actorAvatar,
			SubjectLine:   subject,
			Title:         subject,
			Subtitle:      "Task status",
			Body:          trimSnippet(body, 300),
			CTAURL:        frontendTaskURL(taskUUID),
			CTAText:       "Open task",
			DedupKeyParts: []string{"status", taskUUID, changeID},
			Recipients:    recipientsFromStrings(recipientUUIDs),
		})
	}()
}

// DispatchTaskComment fires for new task comments. Recipients are the
// already-computed set: previous commenters + assignee + mentioned users.
func DispatchTaskComment(actorUUID, actorName, actorAvatar string,
	taskUUID, taskName, plainTextBody string,
	commentUUID string, recipientUUIDs []string) {

	if len(recipientUUIDs) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchTaskComment")
		ctx := context.Background()
		subject := actorName + " commented on " + taskName
		Dispatch(ctx, Event{
			Type:          EventTaskComment,
			ActorUUID:     actorUUID,
			ActorName:     actorName,
			ActorAvatar:   actorAvatar,
			SubjectLine:   subject,
			Title:         subject,
			Subtitle:      "Task comment",
			Body:          trimSnippet(plainTextBody, 600),
			CTAURL:        frontendTaskURL(taskUUID),
			CTAText:       "Open task",
			DedupKeyParts: []string{"task_comment", taskUUID, commentUUID},
			Recipients:    recipientsFromStrings(recipientUUIDs),
		})
	}()
}

// DispatchPostComment fires for new comments on a channel post.
func DispatchPostComment(actorUUID, actorName, actorAvatar string,
	channelUUID, postUUID, plainTextBody, commentUUID string,
	recipientUUIDs []string) {

	if len(recipientUUIDs) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchPostComment")
		ctx := context.Background()
		subject := actorName + " replied to a post"
		Dispatch(ctx, Event{
			Type:          EventPostComment,
			ActorUUID:     actorUUID,
			ActorName:     actorName,
			ActorAvatar:   actorAvatar,
			SubjectLine:   subject,
			Title:         subject,
			Subtitle:      "Post comment",
			Body:          trimSnippet(plainTextBody, 600),
			CTAURL:        frontendChannelPostURL(channelUUID, postUUID),
			CTAText:       "View thread",
			DedupKeyParts: []string{"post_comment", postUUID, commentUUID},
			Recipients:    recipientsFromStrings(recipientUUIDs),
		})
	}()
}

// DispatchDocComment fires for new doc comments. Recipients are the doc
// owner + reading/editing/commenting users (computed by the caller).
func DispatchDocComment(actorUUID, actorName, actorAvatar string,
	docUUID, docTitle, plainTextBody, commentUUID string,
	recipientUUIDs []string) {

	if len(recipientUUIDs) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchDocComment")
		ctx := context.Background()
		subject := actorName + " commented on " + docTitle
		Dispatch(ctx, Event{
			Type:          EventDocComment,
			ActorUUID:     actorUUID,
			ActorName:     actorName,
			ActorAvatar:   actorAvatar,
			SubjectLine:   subject,
			Title:         subject,
			Subtitle:      "Doc comment",
			Body:          trimSnippet(plainTextBody, 600),
			CTAURL:        frontendDocURL(docUUID),
			CTAText:       "View doc",
			DedupKeyParts: []string{"doc_comment", docUUID, commentUUID},
			Recipients:    recipientsFromStrings(recipientUUIDs),
		})
	}()
}

// DispatchChatComment fires for new comments on a 1:1 / group chat message.
func DispatchChatComment(actorUUID, actorName, actorAvatar string,
	grpId, chatUUID, plainTextBody, commentUUID string,
	recipientUUIDs []string) {

	if len(recipientUUIDs) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchChatComment")
		ctx := context.Background()
		subject := actorName + " replied in a chat thread"
		Dispatch(ctx, Event{
			Type:          EventChatComment,
			ActorUUID:     actorUUID,
			ActorName:     actorName,
			ActorAvatar:   actorAvatar,
			SubjectLine:   subject,
			Title:         subject,
			Subtitle:      "Chat reply",
			Body:          trimSnippet(plainTextBody, 600),
			CTAURL:        frontendChatURL(grpId),
			CTAText:       "View chat",
			DedupKeyParts: []string{"chat_comment", chatUUID, commentUUID},
			Recipients:    recipientsFromStrings(recipientUUIDs),
		})
	}()
}

// DispatchChannelCall fires when a video call starts in a channel.
func DispatchChannelCall(actorUUID, actorName, actorAvatar string,
	channelUUID, channelName string,
	recipientUUIDs []string) {

	if len(recipientUUIDs) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchChannelCall")
		ctx := context.Background()
		subject := actorName + " started a call in #" + channelName
		Dispatch(ctx, Event{
			Type:        EventChannelCall,
			ActorUUID:   actorUUID,
			ActorName:   actorName,
			ActorAvatar: actorAvatar,
			SubjectLine: subject,
			Title:       subject,
			Subtitle:    "Live call · #" + channelName,
			Body:        "Join the conversation now.",
			CTAURL:      frontendChannelURL(channelUUID),
			CTAText:     "Join call",
			// Calls are timely; collapse all per-(channel, day) emails into one.
			DedupKeyParts: []string{"channel_call", channelUUID, helpers.NowDateKey()},
			Recipients:    recipientsFromStrings(recipientUUIDs),
		})
	}()
}

// DispatchChatCall fires when a video call starts in a DM/group chat.
func DispatchChatCall(actorUUID, actorName, actorAvatar string,
	grpId string,
	recipientUUIDs []string) {

	if len(recipientUUIDs) == 0 {
		return
	}
	go func() {
		defer recoverDispatch("DispatchChatCall")
		ctx := context.Background()
		subject := actorName + " is calling"
		Dispatch(ctx, Event{
			Type:          EventChatCall,
			ActorUUID:     actorUUID,
			ActorName:     actorName,
			ActorAvatar:   actorAvatar,
			SubjectLine:   subject,
			Title:         subject,
			Subtitle:      "Incoming call",
			Body:          "Tap to join the call.",
			CTAURL:        frontendChatURL(grpId),
			CTAText:       "Join call",
			DedupKeyParts: []string{"chat_call", grpId, helpers.NowDateKey()},
			Recipients:    recipientsFromStrings(recipientUUIDs),
		})
	}()
}

// DispatchMemoryDigestTest sends a one-off TEST digest to a single recipient,
// bypassing the per-(recipient, day) dedup so an admin can re-send and verify
// delivery. It still honors the recipient's email settings + suppression
// downstream in Dispatch. The unique dedup token (nanosecond) guarantees each
// test is delivered rather than collapsed into the daily digest.
func DispatchMemoryDigestTest(recipientUUID, subject, body string) {
	if recipientUUID == "" || body == "" {
		return
	}
	go func() {
		defer recoverDispatch("DispatchMemoryDigestTest")
		ctx := context.Background()
		Dispatch(ctx, Event{
			Type:            EventMemoryDigest,
			SubjectLine:     subject,
			Title:           "Your open items (test)",
			Subtitle:        "From your workspace memory",
			Body:            body,
			CTAURL:          frontendMemoryURL(),
			CTAText:         "Review in OneCamp",
			DedupKeyParts:   []string{"memory_digest_test", strconv.FormatInt(time.Now().UnixNano(), 10)},
			Recipients:      recipientsFromStrings([]string{recipientUUID}),
			SkipOnlineCheck: true,
		})
	}()
}
