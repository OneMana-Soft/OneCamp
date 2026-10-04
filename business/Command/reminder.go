package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	jobDomain "github.com/akashc777/OneCamp/domain/ScheduledJob"
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

	target, rest := parseRemindTarget(text)
	what, whenPhrase, ok := splitWhatWhen(rest)
	if !ok || strings.TrimSpace(what) == "" {
		return errorResponse("Couldn't parse that. Try `/remind me to stretch in 30 minutes`."), nil
	}

	when, err := ParseWhen(whenPhrase, cc.Timezone)
	if err != nil {
		return errorResponse("I couldn't understand “%s”. Try `in 20 minutes`, `tomorrow at 9am`, or `every Monday at 10:00`.", whenPhrase), nil
	}

	// For channel reminders, resolve the channel name → UUID now (while we have
	// the invoking user's context) and verify membership, so the fired job can
	// post a visible message without re-resolving. If the user is in this
	// channel right now, prefer the current channel id.
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

	who := "you"
	if target.kind == "user" {
		who = "@" + target.name
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
		Text:         "⏰ Reminder: " + p.Text,
	}

	// Channel reminder: post a visible message into the channel, authored by
	// the user who set it (Slack parity). Reuses the AI executor's send_message
	// path, which handles permissions, MQTT fan-out, search indexing and
	// notifications. Falls back to nudging the setter if the post fails.
	if p.TargetType == "channel" && p.TargetID != "" {
		if err := postChannelReminder(ctx, p); err != nil {
			logErr(ctx, "runReminderJob/postChannelReminder", err)
			// Fall back: nudge the creator so the reminder isn't silently lost.
			resp.Text = fmt.Sprintf("⏰ Reminder for #%s: %s (couldn't post to the channel)", p.TargetName, p.Text)
			PublishEphemeralToUser(p.CreatedBy, resp)
			pushReminder(ctx, p.CreatedBy, resp.Text)
		}
		return nil
	}

	// Resolve the recipient: explicit target user, else the creator.
	recipient := p.CreatedBy
	if p.TargetType == "user" && p.TargetID != "" {
		recipient = p.TargetID
	}

	// 1. Live clients (open tab / foreground PWA) — ephemeral card.
	PublishEphemeralToUser(recipient, resp)

	// 2. Backgrounded / closed PWA & native — FCM web push so backgrounded
	//    mobile PWAs are woken. Best-effort: never fail the job if push is
	//    unavailable.
	pushReminder(ctx, recipient, resp.Text)

	return nil
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

// parseRemindTarget peels an optional leading "me", "@user" or "#channel".
func parseRemindTarget(text string) (remindTarget, string) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return remindTarget{kind: "self"}, text
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
