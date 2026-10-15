package notification

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/google/uuid"
)

type bookingWays struct {
	devices []string
	pushes  chan map[string]string
	emails  []Event
	queued  int // what the email answers: how many it queued
}

// withBookingSeams stands in the devices, push and email. Email is off unless
// queued is set, as on OneCamp Cloud's essentials-only tier.
func withBookingSeams(t *testing.T, w *bookingWays) {
	t.Helper()
	oldD, oldP, oldE := bookingDevices, bookingPush, bookingEmail
	t.Cleanup(func() { bookingDevices, bookingPush, bookingEmail = oldD, oldP, oldE })
	t.Setenv("RESEND_API_KEY", "")
	w.pushes = make(chan map[string]string, 8)
	bookingDevices = func(_ context.Context, ids []string) ([]string, error) {
		w.devices = append(w.devices, ids...)
		out := []string{}
		for _, id := range ids {
			out = append(out, "phone-of-"+id)
		}
		return out, nil
	}
	bookingPush = func(_ context.Context, data map[string]string, tokens []string) error {
		if len(tokens) != 1 {
			t.Errorf("pushed to %v", tokens)
		}
		w.pushes <- data
		return nil
	}
	bookingEmail = func(ctx context.Context, e Event) int {
		w.emails = append(w.emails, e)
		if w.queued > 0 {
			return w.queued
		}
		return Dispatch(ctx, e) // the real one, with no sending key: queues nothing
	}
}

func (w *bookingWays) push(t *testing.T) map[string]string {
	t.Helper()
	select {
	case p := <-w.pushes:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no push")
		return nil
	}
}

func aBooking(owner string) BookingNotice {
	start := time.Date(2026, 10, 14, 4, 30, 0, 0, time.UTC)
	return BookingNotice{OwnerUUID: owner, EventUUID: uuid.NewString(), BookingID: uuid.NewString(),
		GuestName: "Alex Kim", PageTitle: "Intro call", Start: start, End: start.Add(30 * time.Minute),
		TZ: "Asia/Kolkata", When: "Wednesday 14 October 2026, 10:00–10:30 (Asia/Kolkata)"}
}

// CAL-BUG-5: a booking was told to its owner by email alone, so without
// notification email nobody heard of it, while the guest read "Sam has been
// told". With email off, the owner now gets a push. This edition has no inbox
// item for it, so the guest isn't told the owner was.
func TestABookingReachesItsOwnerWithoutEmail(t *testing.T) {
	w := &bookingWays{}
	withBookingSeams(t, w)
	owner := uuid.NewString()
	n := aBooking(owner)

	if TellBookingOwner(context.Background(), n) {
		t.Error("with no inbox here and no email, the owner can't be counted as told")
	}
	p := w.push(t)
	for k, want := range map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:    firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID: n.EventUUID,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:   "Alex Kim booked Intro call",
		firebaseInit.FIREBASE_PUSH_DATA_BODY:    n.When,
		firebaseInit.FIREBASE_PUSH_DATA_TAG:     "booking-" + n.BookingID,
	} {
		if p[k] != want {
			t.Errorf("push %s = %q, want %q", k, p[k], want)
		}
	}
	if len(w.devices) != 1 || w.devices[0] != owner {
		t.Errorf("devices of %v", w.devices)
	}
	// The email was asked for, and with no sending key it queued nothing.
	if len(w.emails) != 1 || w.emails[0].Type != EventCalendarBooking || !w.emails[0].SkipOnlineCheck {
		t.Errorf("emails %+v", w.emails)
	}
}

func TestACancelledBookingSaysSo(t *testing.T) {
	w := &bookingWays{queued: 1}
	withBookingSeams(t, w)
	n := aBooking(uuid.NewString())
	n.Cancelled = true
	if !TellBookingOwner(context.Background(), n) {
		t.Fatal("an email was queued, so the owner was told")
	}
	if p := w.push(t); p[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Alex Kim cancelled Intro call" {
		t.Errorf("push %v", p)
	}
	if e := w.emails[0]; e.Title != "Booking cancelled" || e.SubjectLine != "Alex Kim cancelled Intro call" {
		t.Errorf("email %+v", e)
	}
	if TellBookingOwner(context.Background(), aBooking("not-a-person")) {
		t.Error("told nobody")
	}
}
