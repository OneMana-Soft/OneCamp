package notification

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type bookingWays struct {
	mu        sync.Mutex
	notices   []*followModels.Notice
	keys      map[string]bool
	live      []string
	pushedTo  []string // who was pushed (Deliver's recipients)
	noticeIDs []string // the desktop notice each push was named
	pushes    chan map[string]string
	emails    []Event
	writeErr  error
	queued    int // what the email answers: how many it queued
}

// withBookingSeams stands in the inbox, the push and the email. Email is off
// unless queued is set, as on OneCamp Cloud's essentials-only tier.
func withBookingSeams(t *testing.T, w *bookingWays) {
	t.Helper()
	oldW, oldL, oldOn := noticeWrite, noticeLive, noticesOn
	oldD, oldE, oldA := noticeDeliver, bookingEmail, noticeActive
	t.Cleanup(func() {
		noticeWrite, noticeLive, noticesOn = oldW, oldL, oldOn
		noticeDeliver, bookingEmail, noticeActive = oldD, oldE, oldA
	})
	// The owner is active.
	noticeActive = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]*userModels.User, error) {
		out := map[uuid.UUID]*userModels.User{}
		for _, id := range ids {
			out[id] = &userModels.User{Id: id}
		}
		return out, nil
	}
	t.Setenv("RESEND_API_KEY", "")
	w.keys = map[string]bool{}
	w.pushes = make(chan map[string]string, 8)
	noticesOn = func() bool { return true }
	noticeWrite = func(_ context.Context, rows []*followModels.Notice) (map[uuid.UUID]bool, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.writeErr != nil {
			return nil, w.writeErr
		}
		out := map[uuid.UUID]bool{}
		for _, n := range rows {
			if w.keys[n.DedupKey] {
				continue
			}
			w.keys[n.DedupKey] = true
			w.notices = append(w.notices, n)
			out[n.UserUUID] = true
		}
		return out, nil
	}
	noticeLive = func(user string, item *dgraphModels.UnifiedActivityItem) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if item.Notice == nil || item.ActivityType != ActivityNotice {
			t.Errorf("live item %+v", item)
		}
		w.live = append(w.live, user)
	}
	noticeDeliver = func(_ context.Context, to []string, data map[string]string, id string) {
		w.mu.Lock()
		w.pushedTo = append(w.pushedTo, to...)
		w.noticeIDs = append(w.noticeIDs, id)
		w.mu.Unlock()
		if len(to) != 1 {
			t.Errorf("pushed to %v", to)
		}
		w.pushes <- data
	}
	bookingEmail = func(ctx context.Context, e Event) int {
		w.mu.Lock()
		w.emails = append(w.emails, e)
		w.mu.Unlock()
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

func (w *bookingWays) noMorePushes(t *testing.T) {
	t.Helper()
	select {
	case p := <-w.pushes:
		t.Errorf("an extra push: %v", p)
	case <-time.After(200 * time.Millisecond):
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
// told". With email off, the owner now gets an inbox item, live, and a push.
func TestABookingReachesItsOwnerWithoutEmail(t *testing.T) {
	w := &bookingWays{}
	withBookingSeams(t, w)
	owner := uuid.NewString()
	n := aBooking(owner)

	if !TellBookingOwner(context.Background(), n) {
		t.Fatal("the owner had an inbox item, so they were told")
	}
	if len(w.notices) != 1 {
		t.Fatalf("notices %d", len(w.notices))
	}
	got := w.notices[0]
	if got.UserUUID.String() != owner || got.Kind != NoticeBooked || got.ResourceKind != "event" ||
		got.ResourceUUID != n.EventUUID || got.Reason != "owner" || got.ActorUUID != "" {
		t.Errorf("notice %+v", got)
	}
	for k, want := range map[string]string{"guest_name": "Alex Kim", "page_title": "Intro call", "booking_id": n.BookingID,
		"starts_at": "2026-10-14T04:30:00Z", "ends_at": "2026-10-14T05:00:00Z", "tz": "Asia/Kolkata"} {
		if got.Data[k] != want {
			t.Errorf("data[%s] = %q, want %q", k, got.Data[k], want)
		}
	}
	if len(w.live) != 1 || w.live[0] != owner {
		t.Errorf("live to %v", w.live)
	}
	p := w.push(t)
	for k, want := range map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:     firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  n.EventUUID,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:    "Alex Kim booked Intro call",
		firebaseInit.FIREBASE_PUSH_DATA_BODY:     n.When,
		firebaseInit.FIREBASE_PUSH_DATA_TAG:      "booking-" + n.BookingID,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME: "Alex Kim",
	} {
		if p[k] != want {
			t.Errorf("push %s = %q, want %q", k, p[k], want)
		}
	}
	if len(w.pushedTo) != 1 || w.pushedTo[0] != owner {
		t.Errorf("pushed to %v", w.pushedTo)
	}
	if want := NoticeBooked + "|" + n.BookingID; len(w.noticeIDs) != 1 || w.noticeIDs[0] != want {
		t.Errorf("the push was noticed as %v, want the telling's key %q", w.noticeIDs, want)
	}
	// The email was asked for, and with no sending key it queued nothing.
	if len(w.emails) != 1 || w.emails[0].Type != EventCalendarBooking || !w.emails[0].SkipOnlineCheck {
		t.Errorf("emails %+v", w.emails)
	}
}

func TestACancelledBookingSaysSo(t *testing.T) {
	w := &bookingWays{}
	withBookingSeams(t, w)
	n := aBooking(uuid.NewString())
	n.Cancelled = true
	if !TellBookingOwner(context.Background(), n) {
		t.Fatal("not told")
	}
	if len(w.notices) != 1 || w.notices[0].Kind != NoticeBookingCancelled {
		t.Fatalf("notices %+v", w.notices)
	}
	if p := w.push(t); p[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Alex Kim cancelled Intro call" {
		t.Errorf("push %v", p)
	}
	if e := w.emails[0]; e.Title != "Booking cancelled" || e.SubjectLine != "Alex Kim cancelled Intro call" {
		t.Errorf("email %+v", e)
	}
}

// The same booking's same change is told once (Tell's rule): told again, it
// sends nothing, and says it told nobody now.
func TestABookingIsToldOnce(t *testing.T) {
	w := &bookingWays{}
	withBookingSeams(t, w)
	n := aBooking(uuid.NewString())
	if !TellBookingOwner(context.Background(), n) {
		t.Fatal("not told")
	}
	w.push(t)
	if TellBookingOwner(context.Background(), n) {
		t.Error("told twice")
	}
	w.noMorePushes(t)
	if len(w.notices) != 1 || len(w.live) != 1 {
		t.Errorf("notices %d, live %d", len(w.notices), len(w.live))
	}
}

// Told is what happened: no inbox item and no email is not told, and with no
// inbox item there is no push either (Tell sends only what it wrote); an
// email queued is told, with no inbox at all.
func TestOwnerToldSaysWhatHappened(t *testing.T) {
	w := &bookingWays{writeErr: errors.New("database down")}
	withBookingSeams(t, w)
	if TellBookingOwner(context.Background(), aBooking(uuid.NewString())) {
		t.Error("nothing reached the owner")
	}
	w.noMorePushes(t)

	w2 := &bookingWays{queued: 1}
	withBookingSeams(t, w2)
	noticesOn = func() bool { return false }
	if !TellBookingOwner(context.Background(), aBooking(uuid.NewString())) {
		t.Error("an email was queued")
	}
	w2.noMorePushes(t)
	if len(w2.notices) != 0 {
		t.Errorf("notices %d with no inbox", len(w2.notices))
	}

	if TellBookingOwner(context.Background(), aBooking("not-a-person")) {
		t.Error("told nobody")
	}
}
