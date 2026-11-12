// Package business (Notify) is the Notification Router
// (design/chat-vs-slack/PROGRAM.md §4.2 #25 and CH6; A4 builds it): one
// decision about who hears of an event, by which delivery and when, for every
// notification in chat and, through their causes, in the calendar.
//
// A4 H1. Plan decides (plan.go), in the design's order
// (design/chat-vs-slack/threads-notifications.md, "The Notification Router",
// steps 1-5): the audience by cause, access, one cause a person, preference,
// and the hold. It reads only, and only in batches (Inputs): one read for the
// whole event, never one a person. Explain is the same plan for the person
// asking, about their own part in it alone.
//
// An event with no Place is a site's: the site worked out who hears
// (Event.Audience), as the calendar does, and Plan answers those people as
// today, each once, never the actor. Deliver is business/Notification
// Deliver, the push and desktop notice through the push gate (pause, quiet
// hours, focus time). Email stays with each site's dispatcher. Only the
// calendar asks Notices yet (business/Calendar notify.go); chat's sites keep
// deciding alone until A4 H2 has them call Plan and Deliver.
//
// Permission model: reads only. Plan decides and sends nothing; Explain
// answers the person asking about their own decisions only, never another
// person's and never a body.
package business

import (
	"context"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Cause is why someone hears of an event: the strongest one wins for each
// person. Never renamed once shipped: delivery records and preferences carry
// it.
type Cause string

const (
	// Chat's causes, one per kind of notice today's sites send.
	CauseMessage  Cause = "chat.message"  // a new message where they hear everything (all_posts)
	CauseMention  Cause = "chat.mention"  // they were mentioned
	CauseReply    Cause = "chat.reply"    // a reply in a thread they're in (thread_reply)
	CauseReaction Cause = "chat.reaction" // a reaction to their message
	CauseCall     Cause = "chat.call"     // a huddle started where they are

	// The router's causes for chat (A4 H1), with the two above under the
	// design's names.
	CauseDM          Cause = "chat.dm"           // a message in a DM or group they're in
	CauseReplyToYou  Cause = "chat.reply_to_you" // an answer to their message: its thread or an inline reply
	CauseThreadReply       = CauseReply          // a reply in a thread they replied in or follow
	CauseTeam        Cause = "chat.team"         // a team they're in was mentioned
	CauseKeyword     Cause = "chat.keyword"      // it holds one of their keywords
	CauseBroadcast   Cause = "chat.broadcast"    // @here, @channel or @everyone
	CauseAllPosts          = CauseMessage        // a message where they hear everything

	// The calendar's causes (design/calendar-vs-gcal/PROGRAM.md §5), declared
	// at its day 0: calendar A2 (invitations, changes, answers, bookings) and
	// A5 (reminders) tell people through Notices with them. Plan answers the
	// site's audience, and email stays with the site, as W0's CAL-BUG-5 fix
	// sends it.
	CauseCalendarInvited   Cause = "calendar.invited"   // asked to an event
	CauseCalendarUpdated   Cause = "calendar.updated"   // an event they're on changed
	CauseCalendarCancelled Cause = "calendar.cancelled" // an event they're on was cancelled
	CauseCalendarRSVP      Cause = "calendar.rsvp"      // a guest answered their invitation
	CauseCalendarReminder  Cause = "calendar.reminder"  // an event they're on starts soon
	CauseCalendarBooking   Cause = "calendar.booking"   // someone booked, moved or cancelled time with them

	// A test notice a person sent themselves from Settings > Notifications
	// (A4 H3, Test): to their own devices alone, held as any notice is, and
	// never with a body.
	CauseTest Cause = "notify.test"
)

// Delivery is one way someone hears of an event.
type Delivery string

const (
	ToInbox   Delivery = "inbox"
	ToBadge   Delivery = "badge"
	ToPush    Delivery = "push"
	ToDesktop Delivery = "desktop"
	ToEmail   Delivery = "email"
	ToSound   Delivery = "sound"
)

// Where chat happens.
const (
	PlaceChannel = "channel"
	PlaceDM      = "dm"
	PlaceGroup   = "group"
)

// Place is where an event happened: a channel, DM or group by id, and the
// thread's root message when it is a reply in one.
type Place struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	ThreadRootID string `json:"thread_root_id,omitempty"`
}

// Event is one thing people may hear of. Bodies never travel on it: Push holds
// the notice as rendered (title, short body, link).
type Event struct {
	// ID names the thing, the same for every copy of it, so a device raises it
	// once: "<type>:<type_id>:<message or thread id>".
	ID    string
	Cause Cause
	// Actor is who did it (a user uuid); they never hear of it.
	Actor string
	// Push is the push's data (firebaseInit's keys). Its short body is the
	// text keywords are matched in, as the sites match them today.
	Push map[string]string
	// Audience is, for an event with no Place, who its site decided hears of
	// it (user uuids), and for a DM or group, the people in it as the site has
	// them. Plan leaves the actor out, as every site does.
	Audience []string
	// Quiet is "Tell guests? Don't send" (D-CAL-5): the inbox alone.
	Quiet bool

	// Chat's events say where and what, and Plan works out who.
	Place Place
	// ActorInfo is the actor as the site has them: a team's mention reaches
	// only through teams they may mention (business/Reach). Never sent.
	ActorInfo *userModels.UserInfo
	// Mentions are the people it names (user uuids), Teams the teams it
	// names, and Broadcast its "here", "channel" or "everyone" ("" for none).
	Mentions  []string
	Teams     []string
	Broadcast string
	// Within, when not nil, bounds whom its teams and broadcast reach: those
	// its write recorded as reached (the message's mention edges, written
	// as business/Reach decided for the writer then), so a plan made after
	// the write never reaches more than the write did.
	Within []string
	// Author is whose message it answers (its thread's root, or the message an
	// inline reply quotes) or a reaction is to.
	Author string
	// Repliers are those who replied in its thread before it.
	Repliers []string
	// Anyway are those the actor chose to tell through their hold (Notify
	// anyway: business/Notification NotifyAnyway decides who may).
	Anyway []string
	// MessageID is the message the event is about (a post, a chat message, a
	// thread reply, or the message a reaction is on): deleting it retracts
	// the event's deliveries (Retract). "" for an event about no message.
	MessageID string
}

// Decision is one person's part in an event.
type Decision struct {
	UserID string     `json:"user_id"`
	Cause  Cause      `json:"cause"`
	To     []Delivery `json:"to"`
	// Held are the deliveries the person's hold (pause, schedule, focus time)
	// keeps back until HoldTill: email then, push, desktop and sound never.
	Held     []Delivery `json:"held,omitempty"`
	HoldTill *time.Time `json:"hold_till,omitempty"`
	// Note says why someone named is told nothing: NotInPlace for a person
	// mentioned where they aren't (the composer's invite prompt, TN-63).
	Note string `json:"note,omitempty"`
}

// NotInPlace is the note on someone mentioned in a place they can't read.
const NotInPlace = "not_in_place"

// Trace is "why did or didn't I get this?": each planning step's verdict for
// one person, the one asking, and their decision when they have one. It
// never names anyone else and never holds a body.
type Trace struct {
	UserID   string    `json:"user_id"`
	EventID  string    `json:"event_id"`
	Steps    []Step    `json:"steps"`
	Decision *Decision `json:"decision,omitempty"`
	// Deliveries are, for a trace read from the record (Why), each way the
	// event reached the person and where it stands now.
	Deliveries []DeliveryState `json:"deliveries,omitempty"`
}

// DeliveryState is one delivery of an event to the person asking, as the
// router recorded it: "sent", "held" (until HoldUntil), "dropped" (held,
// then its message was deleted) or "retracted" (sent, then deleted). At is
// when it was recorded.
type DeliveryState struct {
	Delivery  Delivery   `json:"delivery"`
	State     string     `json:"state"`
	HoldUntil *time.Time `json:"hold_until,omitempty"`
	At        time.Time  `json:"at"`
}

// Step is one planning step's verdict: Step is "audience", "access",
// "cause", "preference" or "hold", Verdict a code the web app words, and
// Causes the causes the step found or chose.
type Step struct {
	Step    string  `json:"step"`
	Verdict string  `json:"verdict"`
	Causes  []Cause `json:"causes,omitempty"`
}

// Router decides who hears of an event and tells them.
type Router interface {
	// Plan is who hears of ev, and how. It reads only.
	Plan(ctx context.Context, ev Event) ([]Decision, error)
	// Deliver tells each decision's person of ev by its deliveries, each
	// (event, person, delivery) once, and answers what it newly told: the
	// decisions with only the deliveries not recorded before. It never fails
	// the change that caused it: a delivery or a record that fails is logged.
	Deliver(ctx context.Context, ev Event, ds []Decision) []Decision
	// Retract takes back the notices of a deleted message: what was sent is
	// marked retracted, what was still held is dropped and never sent, and
	// the inbox notices it made leave the inbox (soft).
	Retract(ctx context.Context, messageID string)
	// Explain is asker's own part in ev, step by step. It reads only, and
	// only asker's own settings.
	Explain(ctx context.Context, asker string, ev Event) (Trace, error)
}

// Notices is the router everything asks.
var Notices Router = router{in: liveInputs{}, now: time.Now}

// deliver is business/Notification Deliver (a test swaps it).
var deliver = notificationBusiness.Deliver

// router is the Notification Router: Plan and Explain (plan.go) over its
// inputs, and today's delivery.
type router struct {
	in  Inputs
	now func() time.Time
}
