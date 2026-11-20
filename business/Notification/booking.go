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
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/google/uuid"
)

// The kinds of a booking's inbox items.
const (
	NoticeBooked           = "booking.created"
	NoticeBookingCancelled = "booking.cancelled"
	NoticeBookingMoved     = "booking.moved"
)

// BookingChange is what a guest did to a booking: booked it (the zero
// value), cancelled it, or moved it by its link (gcal-7 H2).
type BookingChange int

const (
	BookingMade BookingChange = iota
	BookingCancelled
	BookingMoved
)

// BookingNotice is what the owner of a booking page is told: a guest booked
// time with them, cancelled it, or moved it.
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
	// TZ is the page's zone, and When the time in words in it: a move's new
	// time.
	TZ     string
	When   string
	Change BookingChange
	// Answers are what the guest answered the page's questions, as plain
	// text lines for the owner's mail: none in the inbox or a push.
	Answers string
	// HostUUIDs are a team page's accepted hosts, told as its owner is.
	HostUUIDs []string
}

// Seam for tests: the email.
var bookingEmail = Dispatch

// TellBookingOwner tells a booking page's owner that a guest booked,
// cancelled or moved, and answers whether they were told: an inbox item was
// written, or an email was queued. The same booking's same change is told
// once; each move is a change of its own, so a booking moved back to a time
// it had is told again. It outlives a guest who closes the page mid-request.
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
	kind, word, heading, body, key := NoticeBooked, "booked", "New booking", n.When, n.BookingID
	switch n.Change {
	case BookingCancelled:
		kind, word, heading = NoticeBookingCancelled, "cancelled", "Booking cancelled"
	case BookingMoved:
		kind, word, heading, body = NoticeBookingMoved, "moved", "Booking moved", "To "+n.When
		key += "|" + n.Start.UTC().Format(time.RFC3339) + "|" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	guest := actorOrSomeone(n.GuestName)
	title := guest + " " + word + " " + n.PageTitle
	// What the guest answered goes by mail alone, under the time.
	mailBody := body
	if n.Answers != "" {
		mailBody += "\n\n" + n.Answers
	}

	// The page's accepted hosts are told as its owner is: it is their meeting too.
	people := []NoticeRecipient{{UserUUID: owner.String(), Reason: "owner"}}
	for _, h := range n.HostUUIDs {
		if id, err := uuid.Parse(h); err == nil && id != owner {
			people = append(people, NoticeRecipient{UserUUID: id.String(), Reason: "host"})
		}
	}
	return tellBookingPeople(ctx, Notice{
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
		DedupKey: kind + "|" + key,
		Title:    title,
		Body:     body,
		Path:     "/app/calendar",
		Push: &Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR, ID: n.EventUUID,
			Tag: "booking-" + n.BookingID, From: guest},
	}, Event{
		Type:        EventCalendarBooking,
		ActorName:   guest,
		SubjectLine: title,
		Title:       heading,
		Subtitle:    n.PageTitle,
		Body:        mailBody,
		CTAURL:      frontendBaseURL() + "/app/calendar",
		CTAText:     "Open your calendar",
		// Email is how an owner away from OneCamp hears, so it isn't held
		// for being online.
		DedupKeyParts: []string{"calendar.booking", key, word},
	}, people)
}

// tellBookingPeople tells people of a booking page's news: an inbox item
// each, and an email, never held for being online. True when anyone was
// told.
func tellBookingPeople(ctx context.Context, n Notice, mail Event, people []NoticeRecipient) bool {
	inbox := len(Tell(ctx, n, people)) > 0
	ids := make([]string, 0, len(people))
	for _, p := range people {
		ids = append(ids, p.UserUUID)
	}
	mail.Recipients = recipientsFromStrings(ids)
	mail.SkipOnlineCheck = true
	return bookingEmail(ctx, mail) > 0 || inbox
}

// NoticeBookingHostInvited is a member asked to host a team booking page.
const NoticeBookingHostInvited = "booking.host_invited"

// HostInvite is members asked to host owner's team page.
type HostInvite struct {
	HostUUIDs []string
	OwnerName string
	PageID    string
	PageTitle string
	// InvitedAt tells one invitation from the next after a leave.
	InvitedAt time.Time
}

// TellBookingHostInvited tells members that a page's owner asked them to
// host it, through the booking notices' one path: one notice for them all.
func TellBookingHostInvited(ctx context.Context, n HostInvite) bool {
	hosts := []NoticeRecipient{}
	for _, h := range n.HostUUIDs {
		if id, err := uuid.Parse(h); err == nil {
			hosts = append(hosts, NoticeRecipient{UserUUID: id.String(), Reason: "host"})
		}
	}
	if len(hosts) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	owner := actorOrSomeone(n.OwnerName)
	title := owner + " asked you to host " + n.PageTitle
	body := "Accept it in your booking pages to be booked with them."
	key := n.PageID + "|" + strconv.FormatInt(n.InvitedAt.UnixNano(), 10)
	return tellBookingPeople(ctx, Notice{
		Kind:         NoticeBookingHostInvited,
		ResourceKind: "booking_page",
		ResourceUUID: n.PageID,
		Data:         map[string]string{"page_id": n.PageID, "page_title": n.PageTitle, "owner_name": n.OwnerName},
		DedupKey:     NoticeBookingHostInvited + "|" + key,
		Title:        title,
		Body:         body,
		Path:         "/app/calendar",
		Push: &Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR, ID: n.PageID,
			Tag: "booking-host-" + n.PageID, From: owner},
	}, Event{
		Type:          EventCalendarBooking,
		ActorName:     owner,
		SubjectLine:   title,
		Title:         "Host a booking page",
		Subtitle:      n.PageTitle,
		Body:          body,
		CTAURL:        frontendBaseURL() + "/app/calendar",
		CTAText:       "Open your calendar",
		DedupKeyParts: []string{"calendar.booking_host", key},
	}, hosts)
}
