package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	savedItemBusiness "github.com/akashc777/OneCamp/business/SavedItem"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	jobDomain "github.com/akashc777/OneCamp/domain/ScheduledJob"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/google/uuid"
)

// reminderPayload is the JSON stored in scheduled_jobs.payload for reminders.
type reminderPayload struct {
	Text       string `json:"text"`
	TargetType string `json:"target_type"` // self | user | channel
	TargetID   string `json:"target_id,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	CreatedBy  string `json:"created_by"`
	Timezone   string `json:"timezone,omitempty"`
}

// init wires the reminder job handler into the scheduler. The scheduler package
// does not import this package, so registration flows one way (Command →
// Scheduler), avoiding an import cycle.
func init() {
	schedulerBusiness.RegisterJobHandler(jobModel.JobTypeReminder, runReminderJob)
}

// handleRemind parses "/remind [@who|#channel|me] <what> <when>" and schedules
// a durable job. Supports "list" and "help" subcommands like Slack.
func handleRemind(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	text := strings.TrimSpace(cc.Text)

	switch strings.ToLower(text) {
	case "", "help":
		return remindHelp(), nil
	case "list":
		return remindList(ctx, cc)
	}

	target, rest := parseRemindTarget(text, cc.Mentions)
	what, whenPhrase, ok := splitWhatWhen(rest)
	if !ok || strings.TrimSpace(what) == "" {
		return errorResponse("Couldn't parse that. Try `/remind me to stretch in 30 minutes`."), nil
	}

	when, err := ParseWhen(whenPhrase, cc.Timezone)
	if err != nil {
		return errorResponse("I couldn't understand “%s”. Try `in 20 minutes`, `tomorrow at 9am`, or `every Monday at 10:00`.", whenPhrase), nil
	}

	// "@handle" is a person, found now: the reminder goes to them, never back
	// to whoever set it. It used to keep the name only, and when it fired it
	// reminded the person who set it, after promising to remind the other.
	var person *dgraphStruct.DgraphUser
	if target.kind == "user" {
		var to *dgraphStruct.DgraphUser
		var rerr error
		if target.id != "" {
			to, rerr = reachablePerson(ctx, target.id)
		} else {
			to, rerr = reminderRecipient(ctx, target.name)
		}
		if rerr != nil {
			return errorResponse("Couldn't look up @%s just now. Please try again.", target.name), nil
		}
		if to == nil {
			return errorResponse("Couldn't find @%s to remind. Use their handle, like `/remind @maya to review the deck tomorrow at 9am`.", target.name), nil
		}
		if to.Uuid == cc.User.UserDgraphInfo.Uuid {
			target = remindTarget{kind: "self"}
		} else {
			target.id, person = to.Uuid, to
		}
	}

	// For channel reminders, resolve the channel name → UUID now (while we have
	// the invoking user's context) and check they may post there, so the fired
	// job can post a visible message without re-resolving. "#here" is the
	// channel the command was typed in.
	if target.kind == "channel" {
		resolvedID, rerr := resolveChannelForReminder(ctx, cc, target.name)
		if rerr != nil || resolvedID == "" {
			return errorResponse("Couldn't find a channel named #%s that you can post to.", target.name), nil
		}
		target.id = resolvedID
	}

	payload := reminderPayload{
		Text:       what,
		TargetType: target.kind,
		TargetID:   target.id,
		TargetName: target.name,
		CreatedBy:  cc.User.UserDgraphInfo.Uuid,
		Timezone:   cc.Timezone,
	}
	payloadJSON, _ := json.Marshal(payload)

	var recurrence *string
	if when.Recurrence != "" {
		recurrence = &when.Recurrence
	}

	ownerUUID, _ := uuid.Parse(cc.User.UserDgraphInfo.Uuid)
	_, err = schedulerBusiness.Enqueue(ctx, schedulerBusiness.EnqueueInput{
		JobType:     jobModel.JobTypeReminder,
		UserUUID:    ownerUUID,
		PayloadJSON: string(payloadJSON),
		RunAt:       when.At.UTC(),
		Recurrence:  recurrence,
	})
	if err != nil {
		return errorResponse("Couldn't save that reminder. Please try again."), nil
	}

	// Named as found, so the person setting it sees who will be reminded.
	who := "you"
	if target.kind == "user" {
		who = person.DisplayName()
	} else if target.kind == "channel" {
		who = "#" + target.name
	}
	suffix := ""
	if when.Recurrence != "" {
		suffix = " (recurring)"
	}
	return ephemeral(fmt.Sprintf("⏰ Okay! I'll remind %s “%s” %s%s.", who, what, when.Display, suffix)), nil
}

// runReminderJob fires when a reminder's run_at arrives. It delivers the nudge
// to the target across every surface: an ephemeral card over MQTT for live
// clients, plus an FCM web push so backgrounded mobile PWAs are woken. The two
// paths are complementary — the service worker shows the push when the tab is
// closed; the MQTT card shows when it's open.
func runReminderJob(ctx context.Context, job *jobModel.ScheduledJob) error {
	var p reminderPayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		// Bad payload — don't retry forever.
		logErr(ctx, "runReminderJob/unmarshal", err)
		return nil
	}

	resp := &commandAdapter.CommandResponse{
		ResponseType: "ephemeral",
		Ephemeral:    true,
		Kind:         commandAdapter.KindReminder,
		Text:         "⏰ Reminder: " + p.Text,
	}

	// Channel reminder: post a visible message into the channel, authored by
	// the user who set it (Slack parity), through the composer's own rules and
	// path (business/Send). Falls back to nudging the setter if the post fails.
	if p.TargetType == "channel" && p.TargetID != "" {
		err := postChannelReminder(ctx, p)
		if err == nil {
			return nil
		}
		if rj, ok := sendBusiness.AsRejection(err); ok {
			// The rules say no (they left the channel, it was archived, only
			// its admins post now), and would say no every time: the
			// reminder stops, a repeating one included, and its owner hears.
			resp.Text = fmt.Sprintf("⏰ Reminder for #%s: %s (not posted: you can't post there any more, so this reminder has stopped)", p.TargetName, p.Text)
			deliverReminder(ctx, job, p.CreatedBy, resp)
			return &schedulerBusiness.Terminal{Reason: "Not posted: " + rj.Msg}
		}
		logErr(ctx, "runReminderJob/postChannelReminder", err)
		// Fall back: nudge the creator so the reminder isn't silently lost.
		resp.Text = fmt.Sprintf("⏰ Reminder for #%s: %s (couldn't post to the channel)", p.TargetName, p.Text)
		deliverReminder(ctx, job, p.CreatedBy, resp)
		return nil
	}

	// A reminder for someone else goes to them, saying who asked, while they
	// can still be written to; otherwise it comes back to whoever set it,
	// saying so, rather than being lost.
	recipient := p.CreatedBy
	if p.TargetType == "user" {
		to, err := reminderTargetNow(ctx, p)
		if err != nil {
			return err // the scheduler tries again
		}
		if to != nil && to.Uuid != p.CreatedBy {
			recipient = to.Uuid
			resp.Text = fmt.Sprintf("⏰ %s asked me to remind you: %s", reminderSetterName(ctx, p.CreatedBy), p.Text)
		} else if to == nil {
			resp.Text = fmt.Sprintf("⏰ Reminder for @%s: %s (not delivered: I couldn't reach @%s, so it came to you)", p.TargetName, p.Text, p.TargetName)
		}
	}

	deliverReminder(ctx, job, recipient, resp)
	return nil
}

// deliverReminder gives a fired reminder to the person with userUUID: first a
// Later item, due now, where it waits whatever else reaches them
// (savedItemBusiness.SaveFired); then an ephemeral card for an open tab, and
// a push to wake a closed or backgrounded app. A toast and a push were all
// there was, and the desktop app, an install without Firebase and anyone
// offline lost the reminder. Each step is best effort.
func deliverReminder(ctx context.Context, job *jobModel.ScheduledJob, userUUID string, resp *commandAdapter.CommandResponse) {
	if id, err := uuid.Parse(userUUID); err == nil {
		if _, err := savedItemBusiness.SaveFired(ctx, id, job.Id.String(), resp.Text); err != nil {
			logErr(ctx, "runReminderJob/later", err)
		}
	}
	// Live clients (open tab / foreground PWA): the ephemeral card.
	PublishEphemeralToUser(userUUID, resp)
	// Backgrounded or closed apps: a push.
	pushReminder(ctx, userUUID, resp.Text)
}

// reminderRecipient is the person "@handle" names for a reminder: the active
// member with that handle (users.username, without case) who may be written
// to directly (sendBusiness.MayMessage), and not an agent. Nil, with no
// error, when there is no such person.
func reminderRecipient(ctx context.Context, handle string) (*dgraphStruct.DgraphUser, error) {
	id, err := userDomain.GetActiveUserIDByHandle(ctx, handle)
	if err != nil || id == uuid.Nil {
		return nil, err
	}
	return reachablePerson(ctx, id.String())
}

// reachablePerson is the person with userUUID while they may be written to
// directly and aren't an agent; nil, with no error, otherwise.
func reachablePerson(ctx context.Context, userUUID string) (*dgraphStruct.DgraphUser, error) {
	to, err := userDomain.GetDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if to == nil || to.IsBot || sendBusiness.MayMessage(to) != nil {
		return nil, nil
	}
	return to, nil
}

// reminderTargetNow is who a reminder for a person goes to as it fires: the
// person it was set for, while they can still be written to. One set before
// the person was looked up when it was made holds only their handle, and is
// looked up now. Nil, with no error, when nobody can be reached.
func reminderTargetNow(ctx context.Context, p reminderPayload) (*dgraphStruct.DgraphUser, error) {
	if p.TargetID != "" {
		return reachablePerson(ctx, p.TargetID)
	}
	return reminderRecipient(ctx, p.TargetName)
}

// reminderSetterName is how a reminder names whoever set it: their name, or
// "Someone" when they can't be read.
func reminderSetterName(ctx context.Context, userUUID string) string {
	if u, err := userDomain.GetDgraphUserInfoByUUID(ctx, userUUID); err == nil && u != nil {
		if name := u.DisplayName(); name != "" {
			return name
		}
	}
	return "Someone"
}

func remindHelp() *commandAdapter.CommandResponse {
	return ephemeral(strings.Join([]string{
		"*Reminder help*",
		"• `/remind me to <what> <when>` — remind yourself",
		"• `/remind @person <what> <when>` — remind a teammate",
		"• `/remind #channel <what> <when>` — remind a channel",
		"• `/remind list` — view your reminders",
		"",
		"_when_ can be: `in 20 minutes`, `tomorrow at 9am`, `every Monday at 10:00`.",
	}, "\n"))
}

func remindList(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	ownerUUID, err := uuid.Parse(cc.User.UserDgraphInfo.Uuid)
	if err != nil {
		return errorResponse("Couldn't look up your reminders."), nil
	}
	jobs, err := jobDomain.ListByUser(ctx, ownerUUID, jobModel.JobTypeReminder, []string{jobModel.StatusPending}, 25)
	if err != nil {
		return errorResponse("Couldn't load your reminders."), nil
	}
	if len(jobs) == 0 {
		return ephemeral("You have no upcoming reminders."), nil
	}

	var sb strings.Builder
	sb.WriteString("*Your upcoming reminders*\n")
	for _, j := range jobs {
		var p reminderPayload
		_ = json.Unmarshal([]byte(j.Payload), &p)
		when := j.RunAt.In(loadLocation(cc.Timezone)).Format("Mon, Jan 2 at 3:04 PM")
		rec := ""
		if j.Recurrence != nil && *j.Recurrence != "" {
			rec = " _(recurring)_"
		}
		sb.WriteString(fmt.Sprintf("• “%s” — %s%s\n", p.Text, when, rec))
	}
	return ephemeral(sb.String()), nil
}

// --- parsing helpers ---

type remindTarget struct {
	kind string // self | user | channel
	id   string
	name string
}

// parseRemindTarget peels an optional leading "me", "@person" or "#channel".
// A person picked from the composer's list shows as "@" and their name, which
// can hold spaces; mentions (shown names, lower-cased, to user ids) says who
// it is, and the longest name that starts the text wins. Anything else after
// "@" is one word: a handle.
func parseRemindTarget(text string, mentions map[string]string) (remindTarget, string) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return remindTarget{kind: "self"}, text
	}
	if strings.HasPrefix(text, "@") {
		best := ""
		for name := range mentions {
			name = strings.TrimSpace(name)
			if name == "" || len(name) <= len(best) || len(text) < 1+len(name) || !strings.EqualFold(text[1:1+len(name)], name) {
				continue
			}
			if after := text[1+len(name):]; after == "" || after[0] == ' ' {
				best = name
			}
		}
		if best != "" {
			return remindTarget{kind: "user", id: mentions[best], name: text[1 : 1+len(best)]}, strings.TrimSpace(text[1+len(best):])
		}
	}
	head := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(text, head))
	switch {
	case strings.EqualFold(head, "me"):
		return remindTarget{kind: "self"}, rest
	case strings.HasPrefix(head, "@"):
		return remindTarget{kind: "user", name: strings.TrimPrefix(head, "@")}, rest
	case strings.HasPrefix(head, "#"):
		return remindTarget{kind: "channel", name: strings.TrimPrefix(head, "#")}, rest
	default:
		return remindTarget{kind: "self"}, text
	}
}

// splitWhatWhen separates the reminder body from its time phrase. It finds the
// last time-indicator keyword ("in", "at", "tomorrow", "every", "next", "on",
// "today") and treats everything from there as the "when". Also strips a
// leading "to".
func splitWhatWhen(text string) (what string, when string, ok bool) {
	text = strings.TrimSpace(text)
	lower := strings.ToLower(text)
	keywords := []string{" every ", " tomorrow", " today", " next ", " in ", " at ", " on "}

	bestIdx := -1
	for _, kw := range keywords {
		if idx := strings.LastIndex(lower, kw); idx > bestIdx {
			bestIdx = idx
		}
	}
	if bestIdx == -1 {
		return "", "", false
	}

	what = strings.TrimSpace(text[:bestIdx])
	when = strings.TrimSpace(text[bestIdx:])
	what = strings.TrimSpace(strings.TrimPrefix(what, "to "))
	if strings.HasPrefix(strings.ToLower(what), "to ") {
		what = strings.TrimSpace(what[3:])
	}
	return what, when, true
}

var _ = time.Now // keep time import if trimmed by future edits
