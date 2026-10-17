package notification

// A booking page's owner hears of a booking in the app, not only by email
// (CAL-BUG-5).
//
// A guest booking time with someone, or cancelling it, was told to the owner
// by email alone. Where notification email is off (OneCamp Cloud sends only
// the essentials, and an install may have no sending key) nobody was told
// anything: no inbox item, no push, no live update. The event just appeared on
// the owner's calendar the next time it loaded, while the guest read "Sam has
// been told". Now the owner gets an inbox item, live in their open apps, and
// a push to their devices (the lookup holds back anyone paused, in quiet
// hours or in focus time), all by Tell as every notice is; and the email
// where notification email is on, on its own. TellBookingOwner answers
// whether they were told, so the guest's page says only what happened.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/google/uuid"
)

// The kinds of a booking's inbox items.
const (
	NoticeBooked           = "booking.created"
	NoticeBookingCancelled = "booking.cancelled"
)

// BookingNotice is what the owner of a booking page is told: a guest booked
// time with them, or cancelled it.
type BookingNotice struct {
	OwnerUUID string
	// EventUUID is the event the booking made on the owner's calendar; empty
	// when it couldn't be linked.
	EventUUID string
	BookingID string
	GuestName string
	PageTitle string
	Start     time.Time
	End       time.Time
	// TZ is the page's zone, and When the time in words in it.
	TZ        string
	When      string
	Cancelled bool
}

// Seam for tests: the email.
var bookingEmail = Dispatch

// TellBookingOwner tells a booking page's owner that a guest booked or
// cancelled, and answers whether they were told: an inbox item was written,
// or an email was queued. The same booking's same change is told once. It
// outlives a guest who closes the page mid-request.
//
// The guest is outside the workspace, so the notice has no actor: its data
// carries their name with the page's title and the time, for the inbox to say
// "Alex Kim booked Intro call". It opens the owner's calendar, and a later
// push about the same booking replaces this one.
func TellBookingOwner(ctx context.Context, n BookingNotice) bool {
	owner, err := uuid.Parse(n.OwnerUUID)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	kind, verb, word := NoticeBooked, " booked ", "booked"
	if n.Cancelled {
		kind, verb, word = NoticeBookingCancelled, " cancelled ", "cancelled"
	}
	guest := actorOrSomeone(n.GuestName)
	title := guest + verb + n.PageTitle

	inbox := len(Tell(ctx, Notice{
		Kind:         kind,
		ResourceKind: "event",
		ResourceUUID: n.EventUUID,
		Data: map[string]string{
			"booking_id": n.BookingID,
			"guest_name": n.GuestName,
			"page_title": n.PageTitle,
			"starts_at":  n.Start.UTC().Format(time.RFC3339),
			"ends_at":    n.End.UTC().Format(time.RFC3339),
			"tz":         n.TZ,
		},
		DedupKey: kind + "|" + n.BookingID,
		Title:    title,
		Body:     n.When,
		Path:     "/app/calendar",
		Push: &Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR, ID: n.EventUUID,
			Tag: "booking-" + n.BookingID, From: guest},
	}, []NoticeRecipient{{UserUUID: owner.String(), Reason: "owner"}})) > 0
	subject, heading := title, "New booking"
	if n.Cancelled {
		heading = "Booking cancelled"
	}
	emailed := bookingEmail(ctx, Event{
		Type:        EventCalendarBooking,
		ActorName:   guest,
		SubjectLine: subject,
		Title:       heading,
		Subtitle:    n.PageTitle,
		Body:        n.When,
		CTAURL:      frontendBaseURL() + "/app/calendar",
		CTAText:     "Open your calendar",
		// Email is how an owner away from OneCamp hears, so it isn't held
		// for being online.
		DedupKeyParts:   []string{"calendar.booking", n.BookingID, word},
		Recipients:      recipientsFromStrings([]string{owner.String()}),
		SkipOnlineCheck: true,
	}) > 0
	return inbox || emailed
}
