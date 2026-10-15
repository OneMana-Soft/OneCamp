package notification

// A booking page's owner hears of a booking in the app, not only by email
// (CAL-BUG-5).
//
// A guest booking time with someone, or cancelling it, was told to the owner
// by email alone. Where notification email is off (OneCamp Cloud sends only
// the essentials, and an install may have no sending key) nobody was told
// anything: no inbox item, no push, no live update. The event just appeared on
// the owner's calendar the next time it loaded, while the guest read "Sam has
// been told". Now the owner gets an inbox item, live in their open apps
// (bookingInbox), a push to their devices (the lookup holds back anyone
// paused, in quiet hours or in focus time), and the email where notification
// email is on, each on its own. TellBookingOwner answers whether they were
// told, so the guest's page says only what happened.

import (
	"context"
	"slices"
	"time"

	fcmDomain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
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

// Seams for tests: the owner's devices, the push, and the email.
var (
	bookingDevices = fcmDomain.GetFCMTokenByListOfUserId
	bookingPush    = func(ctx context.Context, data map[string]string, tokens []string) error {
		return firebaseInit.FirebaseApp.MultiCastPush(ctx, data, tokens)
	}
	bookingEmail = Dispatch
)

// TellBookingOwner tells a booking page's owner that a guest booked or
// cancelled, and answers whether they were told: an inbox item was written,
// or an email was queued. The push goes either way. The same booking's same
// change is told once. It outlives a guest who closes the page mid-request.
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

	inbox, fresh := bookingInbox(ctx, owner, kind, n)
	if fresh {
		go pushBooking(owner.String(), title, n)
	}
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

// pushBooking pushes a booking to its owner's devices, the paused-aware
// lookup holding back anyone who shouldn't be disturbed now. A later push
// about the same booking replaces this one (its tag).
func pushBooking(owner, title string, n BookingNotice) {
	defer recoverDispatch("TellBookingOwner push")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tokens, err := bookingDevices(ctx, []string{owner})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/TellBookingOwner: the owner's devices couldn't be read: %+v", err)
		return
	}
	if len(tokens) == 0 {
		return
	}
	data := map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:     firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  n.EventUUID,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:    title,
		firebaseInit.FIREBASE_PUSH_DATA_BODY:     n.When,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME: actorOrSomeone(n.GuestName),
		firebaseInit.FIREBASE_PUSH_DATA_TAG:      "booking-" + n.BookingID,
	}
	// 500 tokens a call is Firebase's limit.
	for batch := range slices.Chunk(tokens, 500) {
		if err := bookingPush(ctx, data, batch); err != nil {
			helpers.LogErrorWithContext(ctx, "notification/TellBookingOwner: the push failed: %+v", err)
			return
		}
	}
}
