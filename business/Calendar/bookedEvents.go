package Calendar

// A booked event keeps its booking in step (CAL-BUG-4).
//
// A booking is an ordinary event on its owner's calendar. When the owner
// deleted it (the panel says "It's removed from everyone's calendar") or moved
// it, the guest heard nothing, the booking stayed active at the old time (the
// guest's link still showed it, with "Cancel booking"), and the page kept
// refusing the old slot to everyone else. The guest turned up to a meeting
// that no longer existed, or at the wrong time.
//
// DeleteEvent and UpdateEvent now end by telling the booking: it is cancelled
// by its host, or moved with its event; the slot is free again at once; and
// the guest is emailed, with the invite's CANCEL or a new REQUEST so their
// calendar follows, where notification email is on. Where it's off the guest
// can't be told, and the owner's calendar says so before they confirm
// (markBookedEvents' guest_told). A meeting that has happened is left as it
// was.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

// eventPeople are the people an event makes busy: its creator and guests.
func eventPeople(ev *dgraphStruct.DgraphEvent) []string {
	var out []string
	if ev.CreatedBy != nil {
		out = append(out, ev.CreatedBy.Uuid)
	}
	for _, p := range ev.Participants {
		if p != nil {
			out = append(out, p.Uuid)
		}
	}
	return out
}

// hostName is the name a guest knows the event's creator by.
func hostName(ev *dgraphStruct.DgraphEvent) string {
	if ev.CreatedBy != nil {
		if name := bookingOwnerName(ev.CreatedBy); name != "" {
			return name
		}
	}
	return "Your host"
}

// pageZone is a page's zone, for the time in words.
func pageZone(p *bookingModel.Page) string {
	var hours WorkingHours
	_ = json.Unmarshal(p.Hours, &hours)
	return hours.TZ
}

// tellGuest emails a booking's guest about a change their host made, without
// holding up the host: their calendar shows it already. Whether email is on is
// decided now, at the change.
func tellGuest(ctx context.Context, m guestMail) {
	if !emailService.NotificationEmailEnabled() {
		return
	}
	helpers.GoSafeNamed("business/Calendar/tellGuest", func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		mailGuest(ctx, m)
	})
}

// bookingEventDeleted is DeleteEvent's last step: the event's people are free
// again, and a booking it was cancels, by its host, telling the guest.
func bookingEventDeleted(ctx context.Context, ev *dgraphStruct.DgraphEvent) {
	if ev == nil {
		return
	}
	pageCache.forgetBusy(eventPeople(ev)...)
	id, err := uuid.Parse(ev.Uuid)
	if err != nil {
		return
	}
	b, err := bookingModel.CancelByEvent(id, time.Now())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/bookingEventDeleted cancel err: %+v", err)
		return
	}
	if b == nil {
		return
	}
	p, err := bookingModel.PageById(b.PageId)
	if err != nil || p == nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/bookingEventDeleted page %s err: %+v", b.PageId, err)
		return
	}
	pageCache.forgetOwner(p.UserId.String())
	host := hostName(ev)
	tellGuest(ctx, guestMail{
		to: b.GuestEmail, name: b.GuestName,
		subject:  fmt.Sprintf("Cancelled: %s with %s", p.Title, host),
		title:    host + " cancelled your meeting",
		subtitle: p.Title,
		body:     FormatWhen(b.StartsAt, b.EndsAt, pageZone(p)),
		ctaText:  "Book another time", ctaURL: bookAgainURL(p, p.Active),
		footer: "If it's in your calendar, the update attached takes it off.",
		invite: bookingInvite(inviteCancel, b, p.Title, host, false, time.Now()),
	})
}

// bookingEventUpdated is UpdateEvent's last step: the event's people may be
// free at other times now, and a booking it was moves with it, telling the
// guest the new time.
func bookingEventUpdated(ctx context.Context, ev *dgraphStruct.DgraphEvent, start, end time.Time) {
	if ev == nil {
		return
	}
	pageCache.forgetBusy(eventPeople(ev)...)
	id, err := uuid.Parse(ev.Uuid)
	if err != nil {
		return
	}
	b, wasStart, wasEnd, err := bookingModel.MoveByEvent(id, start.UTC(), end.UTC())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/bookingEventUpdated move err: %+v", err)
		return
	}
	if b == nil {
		return
	}
	p, err := bookingModel.PageById(b.PageId)
	if err != nil || p == nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/bookingEventUpdated page %s err: %+v", b.PageId, err)
		return
	}
	pageCache.forgetOwner(p.UserId.String())
	if !b.EndsAt.After(time.Now()) {
		return // moved into the past: the record keeps it, nobody is told
	}
	host, zone := hostName(ev), pageZone(p)
	tellGuest(ctx, guestMail{
		to: b.GuestEmail, name: b.GuestName,
		subject:  fmt.Sprintf("Moved: %s with %s", p.Title, host),
		title:    host + " moved your meeting",
		subtitle: p.Title,
		body:     "Now " + FormatWhen(b.StartsAt, b.EndsAt, zone) + ". It was " + FormatWhen(wasStart, wasEnd, zone) + ".",
		ctaText:  "Cancel this booking", ctaURL: cancelURL(b.CancelToken),
		footer: "If the new time doesn't suit you, cancel it with the button above. If it's in your calendar, the update attached moves it.",
		invite: bookingInvite(inviteRequest, b, p.Title, host, false, time.Now()),
	})
}

// markBookedEvents is GetEventsList's last step: the reader's own events that
// a guest booked, still to come, say who booked them, through which page, and
// whether the guest would be told of a change, so the calendar can ask before
// one is deleted or moved. One query an answer, and only for the reader's own.
func markBookedEvents(ctx context.Context, reader *model.UserInfo, events []*dgraphStruct.DgraphEvent) {
	if reader == nil || reader.UserDgraphInfo.Uuid == "" || len(events) == 0 {
		return
	}
	mine := map[string]*dgraphStruct.DgraphEvent{}
	for _, e := range events {
		if e != nil && e.CreatedBy != nil && e.CreatedBy.Uuid == reader.UserDgraphInfo.Uuid {
			mine[e.Uuid] = e
		}
	}
	owner, err := uuid.Parse(reader.UserDgraphInfo.Uuid)
	if len(mine) == 0 || err != nil {
		return
	}
	booked, err := bookingModel.UpcomingBookedEvents(owner, time.Now())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/markBookedEvents err: %+v", err)
		return
	}
	told := emailService.NotificationEmailEnabled()
	for _, b := range booked {
		if e := mine[b.EventUUID.String()]; e != nil {
			e.Booking = &dgraphStruct.EventBooking{GuestName: b.GuestName, PageTitle: b.PageTitle, GuestTold: told}
		}
	}
}
