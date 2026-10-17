// Package business (Notify) is the Notification Router
// (design/chat-vs-slack/PROGRAM.md §4.2 #25 and CH6; A4 builds it): one
// decision about who hears of an event, by which delivery and when, for every
// notification in chat and, through their causes, in the calendar.
//
// DAY 0. The router decides nothing of its own. Plan answers the recipients
// the event's site worked out, as each site does today (Event.Audience), and
// Deliver is business/Notification Deliver, the helper chat W0's fixer G built
// for push and the desktop notice and the chat audit fixer made the push gate's
// (pause, quiet hours, focus time): the site's people, each told once by push
// and desktop notice if the gate lets them through. Email stays with each
// site's dispatcher. Nothing calls Notices yet, so nothing changes by its
// existing; A4 replaces Plan with the seven planning steps and folds the
// sites' recipient code away at its wiring.
package business

import (
	"context"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
)

// Cause is why someone hears of an event: the strongest one wins for each
// person. Never renamed once shipped: delivery records and preferences carry
// it.
type Cause string

const (
	// Chat's causes, one per kind of notice today's sites send.
	CauseMessage  Cause = "chat.message"  // a new message where they hear everything, or a DM
	CauseMention  Cause = "chat.mention"  // they were mentioned
	CauseReply    Cause = "chat.reply"    // a reply in a thread they're in
	CauseReaction Cause = "chat.reaction" // a reaction to their message
	CauseCall     Cause = "chat.call"     // a huddle started where they are

	// The calendar's causes (design/calendar-vs-gcal/PROGRAM.md §5), declared
	// at its day 0: calendar A2 (invitations, changes, answers, bookings) and
	// A5 (reminders) tell people through Notices with them. Until A4 replaces
	// Plan, that is the site's audience by push and desktop notice, and email
	// stays with the site, as W0's CAL-BUG-5 fix sends it.
	CauseCalendarInvited   Cause = "calendar.invited"   // asked to an event
	CauseCalendarUpdated   Cause = "calendar.updated"   // an event they're on changed
	CauseCalendarCancelled Cause = "calendar.cancelled" // an event they're on was cancelled
	CauseCalendarRSVP      Cause = "calendar.rsvp"      // a guest answered their invitation
	CauseCalendarReminder  Cause = "calendar.reminder"  // an event they're on starts soon
	CauseCalendarBooking   Cause = "calendar.booking"   // someone booked, moved or cancelled time with them
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

// Event is one thing people may hear of. Bodies never travel on it: Push holds
// the notice as rendered (title, short body, link).
type Event struct {
	// ID names the thing, the same for every copy of it, so a device raises it
	// once: "<type>:<type_id>:<message or thread id>".
	ID    string
	Cause Cause
	// Actor is who did it (a user uuid); they never hear of it.
	Actor string
	// Push is the push's data (firebaseInit's keys).
	Push map[string]string
	// Audience is, until A4, who the event's site decided hears of it: the
	// recipients today's code works out (user uuids). Plan leaves the actor
	// out, as every site does.
	Audience []string
}

// Decision is one person's part in an event.
type Decision struct {
	UserID string     `json:"user_id"`
	Cause  Cause      `json:"cause"`
	To     []Delivery `json:"to"`
}

// Router decides who hears of an event and tells them.
type Router interface {
	// Plan is who hears of ev, and how. It reads only.
	Plan(ctx context.Context, ev Event) ([]Decision, error)
	// Deliver tells each decision's person of ev by its deliveries. It never
	// fails the change that caused it: a delivery that fails is logged.
	Deliver(ctx context.Context, ev Event, ds []Decision)
}

// Notices is the router everything asks.
var Notices Router = todaysRouter{}

// deliver is business/Notification Deliver (a test swaps it).
var deliver = notificationBusiness.Deliver

// todaysRouter is today's notices: the site's audience, by push and desktop.
type todaysRouter struct{}

func (todaysRouter) Plan(_ context.Context, ev Event) ([]Decision, error) {
	ds := make([]Decision, 0, len(ev.Audience))
	for _, id := range ev.Audience {
		if id == "" || id == ev.Actor {
			continue
		}
		ds = append(ds, Decision{UserID: id, Cause: ev.Cause, To: []Delivery{ToPush, ToDesktop}})
	}
	return ds, nil
}

func (todaysRouter) Deliver(ctx context.Context, ev Event, ds []Decision) {
	var told []string
	for _, d := range ds {
		for _, to := range d.To {
			if to == ToPush || to == ToDesktop {
				told = append(told, d.UserID)
				break
			}
		}
	}
	if len(told) > 0 {
		deliver(ctx, told, ev.Push, ev.ID)
	}
}
