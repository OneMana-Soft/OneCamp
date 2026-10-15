package Calendar

// A booking's calendar invite (CAL-BUG-15).
//
// The confirmation page said "The invite is in your email", and the email had
// no invite: a title, the time and a cancel button. The guest's calendar never
// got the meeting unless they pressed "Add to calendar" on the page. Now every
// email to a guest carries an iCalendar invite (RFC 5545), sent the way of
// iMIP (RFC 6047): METHOD REQUEST when booked or moved, CANCEL when
// cancelled. Gmail adds it to the guest's calendar, and Outlook offers to.
//
// All of a booking's invites share one UID, the one the confirmation page's
// own .ics uses (<booking id>@onecamp), with a SEQUENCE that only grows, so a
// moved or cancelled meeting updates the event the guest has, however they
// added it.
//
// The organizer is the owner by name, at the workspace's sending address: the
// booking page never shows the owner's own address, and neither does this.
// The cancel link stays out too. It is the guest's credential, and a calendar
// event is forwarded and shared far more freely than an email.
//
// The iCalendar itself is written by business/ICS.

import (
	"time"

	"github.com/akashc777/OneCamp/business/ICS"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

// The iMIP methods a guest is sent.
const (
	inviteRequest = ICS.Request
	inviteCancel  = ICS.Cancel
)

// invite is one iCalendar message about a booking.
type invite struct {
	Method string
	ICS.Event
}

// bookingInvite is the invite about booking b at now: the confirmation's
// sequence is 0 (fresh is true), and every later one counts the seconds since
// the booking was made, so each is newer than the last.
func bookingInvite(method string, b *bookingModel.Booking, pageTitle, ownerName string, fresh bool, now time.Time) invite {
	seq := 0
	if !fresh {
		seq = 1
		if s := int(now.Sub(b.CreatedAt) / time.Second); !b.CreatedAt.IsZero() && s > seq {
			seq = s
		}
	}
	return invite{Method: method, Event: ICS.Event{
		UID: b.Id.String() + "@onecamp", Sequence: seq, Stamp: now,
		Start: b.StartsAt, End: b.EndsAt,
		Summary:     pageTitle + " with " + ownerName,
		Description: "Booked through " + ownerName + "'s booking page. To cancel, use the link in your confirmation email.",
		Organizer:   ICS.Address{Name: ownerName, Email: emailService.SenderAddress()},
		Attendee:    ICS.Address{Name: b.GuestName, Email: b.GuestEmail},
	}}
}

// attachment is the invite as an email attachment.
func (iv invite) attachment() emailService.Attachment {
	return emailService.Attachment{Filename: "invite.ics", ContentType: "text/calendar; charset=utf-8; method=" + iv.Method,
		Content: iv.ics()}
}

// ics writes the invite. Pure.
func (iv invite) ics() []byte { return ICS.Write(iv.Method, iv.Event) }
