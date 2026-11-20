package Calendar

// A booking's guest is reminded of it by email (gcal-7 H2b): a day and an hour
// before, through the scheduler, with the button to change or cancel it. The
// calendar's own reminders (reminders.go) remind members by user id; a guest
// is only an address, so theirs are planned from the booking.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	scheduledJobDomain "github.com/akashc777/OneCamp/domain/ScheduledJob"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userDgraphModels "github.com/akashc777/OneCamp/models/dgraph/User"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// guestReminderJobType is the scheduler's job that reminds a booking's guest:
// the page owner's job, as the booking is theirs.
const guestReminderJobType = "booking.guest_reminder"

// guestReminderLeads are how long before a booking its guest is reminded.
var guestReminderLeads = []time.Duration{24 * time.Hour, time.Hour}

// guestReminderFloor is the soonest a reminder is planned after the booking
// or move that plans it: a guest who booked inside it has that email still
// fresh. The page's notice is for booking, not for reminding.
const guestReminderFloor = time.Hour

// guestReminder is that job's payload: the booking, the start it was planned
// for, and how long before it. Never the link's secret: the link is opened
// from its sealed copy when the mail goes.
type guestReminder struct {
	BookingID     uuid.UUID `json:"booking_id"`
	Start         time.Time `json:"start"`
	BeforeMinutes int       `json:"before_minutes"`
}

// guestReminderJobs is what reminds booking b's guest, planned at now: a
// reminder per lead whose time is no sooner than guestReminderFloor from now.
// The public demo plans none: it mails nobody outside the workspace
// (outsidersMailed).
func guestReminderJobs(b *bookingModel.Booking, p *bookingModel.Page, now time.Time) []schedulerBusiness.EnqueueInput {
	if helpers.DemoMode() {
		return nil
	}
	earliest := now.Add(guestReminderFloor)
	var jobs []schedulerBusiness.EnqueueInput
	for _, lead := range guestReminderLeads {
		runAt := b.StartsAt.Add(-lead)
		if runAt.Before(earliest) {
			continue
		}
		payload, err := json.Marshal(guestReminder{BookingID: b.Id, Start: b.StartsAt.UTC(), BeforeMinutes: int(lead / time.Minute)})
		if err != nil {
			continue
		}
		// A retry is for a read that failed before the mail; one that went
		// is never retried (fireGuestReminder).
		jobs = append(jobs, schedulerBusiness.EnqueueInput{JobType: guestReminderJobType, UserUUID: p.UserId,
			PayloadJSON: string(payload), RunAt: runAt, MaxAttempts: 3})
	}
	return jobs
}

// planGuestReminders plans booking b's guest reminders after a commit that set
// its time: Book, and a move by its guest or its host (both end in
// bookingEventUpdated). A moved booking's pending reminders are cancelled
// first, softly, so each lead has one live job; one already running when it
// moved finds the new start and sends nothing (remindGuest). Best effort: a
// booking stands without its reminders.
func planGuestReminders(ctx context.Context, b *bookingModel.Booking, p *bookingModel.Page, moved bool, now time.Time) {
	if moved {
		cancelGuestReminders(ctx, p.UserId, b.Id)
	}
	for _, in := range guestReminderJobs(b, p, now) {
		if _, err := schedulerBusiness.Enqueue(ctx, in); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar/planGuestReminders enqueue %s err: %+v", b.Id, err)
		}
	}
}

// cancelGuestReminders cancels, softly, booking id's pending reminders, which
// are its page owner's jobs: when it moves (planGuestReminders) and when its
// guest or its host cancels it (CancelBooking, bookingEventDeleted). One
// UPDATE. Best effort: one left fires and finds the booking gone (reminderDue).
func cancelGuestReminders(ctx context.Context, owner, id uuid.UUID) {
	if _, err := scheduledJobDomain.CancelPendingFor(ctx, owner, guestReminderJobType, "booking_id", id.String()); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/cancelGuestReminders %s err: %+v", id, err)
	}
}

// reminderDue is whether reminder r still stands for booking b at now: b is
// live and still starts when r was planned, and r isn't stale by the members'
// rule (reminderStale): more than reminderLate past its time, or from b's
// start. A cancelled or moved booking's reminder, or one found late after
// downtime, sends nothing.
func reminderDue(b *bookingModel.Booking, r guestReminder, now time.Time) bool {
	return b != nil && b.CancelledAt == nil && b.StartsAt.Equal(r.Start) &&
		!reminderStale(r.Start.Add(-time.Duration(r.BeforeMinutes)*time.Minute), r.Start, now)
}

// remindGuest mails reminder r of booking b, on page p of its owner, to the
// address the guest booked with, and says whether it went. It goes while r
// still stands at now, and once per booking, start and lead, whoever fires it again
// (a retry, a second worker, a job left from a move away and back): the first
// firing claims it in Redis, or in this process while Redis can't be asked.
func remindGuest(ctx context.Context, b *bookingModel.Booking, p *bookingModel.Page, owner string, r guestReminder, now time.Time) bool {
	// Whether it still stands, and the demo's own rule for outsiders
	// (outsidersMailed; mailGuest checks the rest), before the claim, so a
	// stale or demo firing claims nothing.
	if !reminderDue(b, r, now) || p == nil || helpers.DemoMode() {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, redisStore.LimitAskTimeout)
	claim := redisStore.AllowFixedWindowOrLocal(rctx, registry.BookingGuestReminderSent,
		[]string{b.Id.String(), strconv.FormatInt(r.Start.Unix(), 10), strconv.Itoa(r.BeforeMinutes)}, 1)
	cancel()
	if !claim.Allowed {
		return false
	}
	// As the host's move mails it (bookingEventUpdated): a link from before
	// links were sealed, or one the server's key no longer opens, isn't sent.
	ctaText, link, changeBy := "", "", "the link you got when you booked"
	if raw := helpers.OpenLinkSecret(b.CancelSealed, b.CancelHash); raw != "" {
		ctaText, link, changeBy = "Change or cancel", cancelURL(raw), "the button above"
	}
	return mailGuest(ctx, guestMail{
		to: b.GuestEmail, name: b.GuestName,
		subject:  fmt.Sprintf("Reminder: %s with %s", p.Title, owner),
		title:    fmt.Sprintf("Coming up: your meeting with %s", owner),
		subtitle: p.Title,
		body:     FormatWhen(b.StartsAt, b.EndsAt, guestZone(b, p)),
		ctaText:  ctaText, ctaURL: link,
		footer: "You booked this time through a OneCamp booking page. To change or cancel it, use " + changeBy + ".",
		invite: bookingInvite(inviteRequest, b, p.Title, owner, false, time.Now()),
	})
}

// fireGuestReminder is the job's handler. A reminder that no longer stands,
// or whose host has left the workspace, ends with no mail; one that couldn't
// be read is retried; one whose mail didn't go (email off, a send that
// failed) is not, so a mail outage never becomes a retry storm.
func fireGuestReminder(ctx context.Context, job *jobModel.ScheduledJob) error {
	var r guestReminder
	if err := json.Unmarshal([]byte(job.Payload), &r); err != nil || r.BookingID == uuid.Nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: a guest reminder job %s can't be read: %v", job.Id, err)
		return nil
	}
	b, err := bookingModel.BookingByID(ctx, r.BookingID)
	if err != nil {
		return err
	}
	now := time.Now()
	if !reminderDue(b, r, now) {
		return nil
	}
	p, err := bookingModel.PageById(b.PageId)
	if err != nil {
		return err
	}
	// Only the page owner's own job reminds of its bookings.
	if p == nil || p.UserId != job.UserUuid {
		return nil
	}
	du, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.UserId.String())
	if err != nil && !errors.Is(err, userDgraphModels.ErrNotFound) {
		return err
	}
	if du == nil || helpers.IsSoftDeleted(du.DeletedAt) {
		return nil
	}
	remindGuest(ctx, b, p, bookingOwnerName(du), r, now)
	return nil
}

func init() {
	schedulerBusiness.RegisterJobHandler(guestReminderJobType, fireGuestReminder)
}
